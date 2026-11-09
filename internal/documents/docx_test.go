package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

func docxParts(body string) map[string]string {
	return map[string]string{
		"[Content_Types].xml": `<Types xmlns="` + typesNS + `"><Override PartName="/word/document.xml" ContentType="` + mainType + `"/></Types>`,
		"_rels/.rels":         `<Relationships xmlns="` + relsNS + `"><Relationship Id="main" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml":   `<w:document xmlns:w="` + wordNS + `"><w:body>` + body + `</w:body></w:document>`,
	}
}
func packageBytes(t interface {
	Helper()
	Fatal(...any)
}, parts map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	keys := make([]string, 0, len(parts))
	for name := range parts {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		file, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(parts[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestRegressionDOCXBodyPreservesTextAndOmitsOtherStoriesAndFields(t *testing.T) {
	parts := docxParts(`<w:p><w:r><w:t>First &amp; second</w:t><w:tab/><w:t>cell</w:t><w:br/><w:t>next</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Table 中 🧠</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
<w:p><w:del><w:r><w:t>deleted-private</w:t></w:r></w:del><w:moveFrom><w:r><w:t>moved-private</w:t></w:r></w:moveFrom><w:ins><w:r><w:t>Inserted</w:t></w:r></w:ins></w:p>
<w:p><w:r><w:fldChar w:fldCharType="begin"/></w:r><w:r><w:instrText>field-instruction</w:instrText></w:r><w:r><w:fldChar w:fldCharType="separate"/><w:t>field-cached-result</w:t><w:fldChar w:fldCharType="end"/></w:r><w:fldSimple w:instr="command"><w:r><w:t>simple-field-result</w:t></w:r></w:fldSimple></w:p>
<w:p><w:r><w:drawing><w:p><w:r><w:t>drawing-private</w:t></w:r></w:p></w:drawing><w:pict><w:p><w:r><w:t>picture-private</w:t></w:r></w:p></w:pict></w:r></w:p>`)
	parts["word/comments.xml"] = "invalid XML comments-private"
	parts["word/header1.xml"] = "invalid XML header-private"
	parts["word/footnotes.xml"] = "invalid XML notes-private"
	text, err := DOCX(context.Background(), packageBytes(t, parts))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"First & second\tcell\nnext\n", "Table 中 🧠\n", "Inserted\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("lost body structure: %q", text)
		}
	}
	for _, forbidden := range []string{"deleted-private", "moved-private", "field-instruction", "field-cached-result", "simple-field-result", "drawing-private", "picture-private", "comments-private", "header-private", "notes-private"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("unsupported story or field was extracted: %s", forbidden)
		}
	}
}

func TestRegressionDOCXUsesRelationshipIdentityIncludingStrictAndEncodedPaths(t *testing.T) {
	for _, strict := range []bool{false, true} {
		parts := docxParts(`<w:p><w:r><w:t>real-main</w:t></w:r></w:p>`)
		main := parts["word/document.xml"]
		parts["word/document.xml"] = "decoy-is-not-a-main-document"
		parts["custom/main body.xml"] = main
		parts["_rels/.rels"] = strings.ReplaceAll(parts["_rels/.rels"], "word/document.xml", "custom/main%20body.xml")
		parts["[Content_Types].xml"] = strings.ReplaceAll(parts["[Content_Types].xml"], "word/document.xml", "custom/main%20body.xml")
		if strict {
			parts["custom/main body.xml"] = "\xef\xbb\xbf" + strings.ReplaceAll(main, wordNS, strictWordNS)
			parts["_rels/.rels"] = strings.ReplaceAll(parts["_rels/.rels"], "http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument", "http://purl.oclc.org/ooxml/officeDocument/relationships/officeDocument")
		}
		text, err := DOCX(context.Background(), packageBytes(t, parts))
		if err != nil || text != "real-main\n" {
			t.Fatalf("wrong relationship target: %q %v", text, err)
		}
	}
}

func TestRegressionDOCXRejectsUnsafeOrAmbiguousPackages(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(map[string]string)
	}{
		{"missing relationships", func(p map[string]string) { delete(p, "_rels/.rels") }},
		{"missing main", func(p map[string]string) { delete(p, "word/document.xml") }},
		{"invalid main", func(p map[string]string) { p["word/document.xml"] = "invalid" }},
		{"macro main type", func(p map[string]string) {
			p["[Content_Types].xml"] = strings.ReplaceAll(p["[Content_Types].xml"], mainType, "application/vnd.ms-word.document.macroEnabled.main+xml")
		}},
		{"path traversal", func(p map[string]string) { p["../escape"] = "no" }},
		{"absolute entry", func(p map[string]string) { p["/absolute"] = "no" }},
		{"backslash entry", func(p map[string]string) { p[`word\extra`] = "no" }},
		{"case collision", func(p map[string]string) { p["WORD/document.xml"] = "no" }},
		{"external main", func(p map[string]string) {
			p["_rels/.rels"] = strings.ReplaceAll(p["_rels/.rels"], `Target="word/document.xml"`, `Target="https://example.invalid/document.xml" TargetMode="External"`)
		}},
		{"encoded traversal", func(p map[string]string) {
			p["_rels/.rels"] = strings.ReplaceAll(p["_rels/.rels"], "word/document.xml", "%2e%2e/escape")
		}},
		{"main query", func(p map[string]string) {
			p["_rels/.rels"] = strings.ReplaceAll(p["_rels/.rels"], "word/document.xml", "word/document.xml?")
		}},
		{"duplicate relationship", func(p map[string]string) {
			p["_rels/.rels"] = strings.ReplaceAll(p["_rels/.rels"], "</Relationships>", `<Relationship Id="other" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`)
		}},
		{"duplicate type", func(p map[string]string) {
			p["[Content_Types].xml"] = strings.ReplaceAll(p["[Content_Types].xml"], "</Types>", `<Override PartName="/word/document.xml" ContentType="`+mainType+`"/></Types>`)
		}},
		{"directive", func(p map[string]string) {
			p["word/document.xml"] = `<!DOCTYPE document [<!ENTITY x SYSTEM "file:///private/value">]>` + p["word/document.xml"]
		}},
		{"unknown entity", func(p map[string]string) {
			p["word/document.xml"] = strings.ReplaceAll(p["word/document.xml"], "hello", "&unknown;")
		}},
		{"multiple roots", func(p map[string]string) { p["word/document.xml"] += p["word/document.xml"] }},
		{"nested body", func(p map[string]string) {
			p["word/document.xml"] = strings.ReplaceAll(p["word/document.xml"], "hello", `</w:t></w:r><w:body/><w:r><w:t>no`)
		}},
		{"field imbalance", func(p map[string]string) {
			p["word/document.xml"] = strings.ReplaceAll(p["word/document.xml"], "hello", `</w:t><w:fldChar w:fldCharType="begin"/><w:t>no`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parts := docxParts(`<w:p><w:r><w:t>hello</w:t></w:r></w:p>`)
			test.modify(parts)
			if _, err := DOCX(context.Background(), packageBytes(t, parts)); err == nil {
				t.Fatal("unsafe/ambiguous document accepted")
			}
		})
	}
	for _, mode := range []fs.FileMode{fs.ModeSymlink | 0600, fs.ModeNamedPipe | 0600} {
		var data bytes.Buffer
		writer := zip.NewWriter(&data)
		header := &zip.FileHeader{Name: "unsafe"}
		header.SetMode(mode)
		if _, err := writer.CreateHeader(header); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := DOCX(context.Background(), data.Bytes()); err == nil {
			t.Fatal("non-regular archive entry accepted")
		}
	}
}

func TestDOCXParserBudgetsAndCancellation(t *testing.T) {
	if _, err := DOCX(context.Background(), make([]byte, MaxDOCXBytes+1)); err == nil {
		t.Fatal("input bound missing")
	}
	for name, body := range map[string]string{
		"text":      `<w:p><w:r><w:t>` + strings.Repeat("x", MaxTextBytes+1) + `</w:t></w:r></w:p>`,
		"depth":     strings.Repeat("<w:unknown>", 129) + strings.Repeat("</w:unknown>", 129),
		"tokens":    strings.Repeat("<w:p/>", 100001),
		"XML bytes": strings.Repeat(" ", (4<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DOCX(context.Background(), packageBytes(t, docxParts(body))); err == nil {
				t.Fatal("parser budget missing")
			}
		})
	}
	parts := docxParts("")
	for i := 0; i < 254; i++ {
		parts[fmt.Sprintf("extra/%d", i)] = ""
	}
	if _, err := DOCX(context.Background(), packageBytes(t, parts)); err == nil {
		t.Fatal("entry count bound missing")
	}
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	if _, err := w.CreateRaw(&zip.FileHeader{Name: "large", UncompressedSize64: 33 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := DOCX(context.Background(), data.Bytes()); err == nil {
		t.Fatal("expanded byte bound missing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DOCX(ctx, packageBytes(t, docxParts(""))); err != context.Canceled {
		t.Fatal("cancelled parser did not stop", err)
	}
}

func TestRegressionDOCXRejectsCorruptEncryptedAndDuplicateEntries(t *testing.T) {
	for _, variant := range []string{"crc", "encrypted", "duplicate"} {
		parts := docxParts(`<w:p><w:r><w:t>hello</w:t></w:r></w:p>`)
		var output bytes.Buffer
		writer := zip.NewWriter(&output)
		for name, content := range parts {
			header := &zip.FileHeader{Name: name, Method: zip.Store, CompressedSize64: uint64(len(content)), UncompressedSize64: uint64(len(content)), CRC32: crc32.ChecksumIEEE([]byte(content))}
			if name == "word/document.xml" {
				if variant == "crc" {
					header.CRC32 ^= 1
				} else if variant == "encrypted" {
					header.Flags |= 1
				}
			}
			entry, err := writer.CreateRaw(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
		if variant == "duplicate" {
			if _, err := writer.Create("word/document.xml"); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := DOCX(context.Background(), output.Bytes()); err == nil {
			t.Fatal("unsafe archive accepted", variant)
		}
	}
}

func FuzzDOCX(f *testing.F) {
	f.Add([]byte("not a ZIP"))
	f.Add([]byte("PK\x03\x04"))
	f.Add(packageBytes(f, docxParts(`<w:p><w:r><w:t>seed</w:t></w:r></w:p>`)))
	f.Fuzz(func(t *testing.T, data []byte) {
		text, err := DOCX(context.Background(), data)
		if err == nil && len(text) > MaxTextBytes {
			t.Fatal("unbounded extraction")
		}
	})
}

func FuzzDOCXBodyXML(f *testing.F) {
	f.Add([]byte(docxParts(`<w:p><w:r><w:t>seed 中</w:t></w:r></w:p>`)["word/document.xml"]))
	f.Add([]byte(`<invalid/>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4<<20 {
			return
		}
		text, err := bodyText(context.Background(), data)
		if err == nil && len(text) > MaxTextBytes {
			t.Fatal("unbounded body extraction")
		}
	})
}
