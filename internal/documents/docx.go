// Package documents extracts bounded local document text without executing
// application code, evaluating fields, fetching relationships or unpacking files.
package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxDOCXBytes = 2 << 20
	MaxTextBytes = 128 << 10
	DOCXVersion  = "trace-docx-body-v1"
	wordNS       = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	strictWordNS = "http://purl.oclc.org/ooxml/wordprocessingml/main"
	relsNS       = "http://schemas.openxmlformats.org/package/2006/relationships"
	typesNS      = "http://schemas.openxmlformats.org/package/2006/content-types"
	mainType     = "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"
)

func validPart(name string) bool {
	return name != "" && name != "." && len(name) <= 512 && utf8.ValidString(name) && path.Clean(name) == name && !path.IsAbs(name) && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\:") && !strings.ContainsFunc(name, unicode.IsControl)
}

func partTarget(target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil || strings.ContainsAny(target, "?#") || u.Scheme != "" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("DOCX main part must be an internal package path")
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !validPart(name) {
		return "", fmt.Errorf("DOCX contains an invalid part target")
	}
	return name, nil
}

func readPart(file *zip.File, limit uint64) ([]byte, error) {
	if file == nil || !file.Mode().IsRegular() || file.UncompressedSize64 > limit {
		return nil, fmt.Errorf("DOCX required part is missing or exceeds its budget")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("DOCX part could not be opened")
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil || uint64(len(data)) > limit || uint64(len(data)) != file.UncompressedSize64 {
		return nil, fmt.Errorf("DOCX part is corrupt or exceeds its budget")
	}
	return data, nil
}

// scanXML does not load DTDs or external resources. Limits include
// ignored elements so unsupported markup cannot bypass the parser budget.
func scanXML(ctx context.Context, data []byte, visit func(xml.Token, []xml.Name) error) error {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return fmt.Errorf("DOCX XML must be UTF-8")
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	stack := []xml.Name{}
	roots, tokens := 0, 0
	declaration := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := decoder.Token()
		if err == io.EOF {
			if roots != 1 || len(stack) != 0 {
				return fmt.Errorf("DOCX XML has an incomplete root")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("DOCX XML is malformed")
		}
		tokens++
		if tokens > 200000 {
			return fmt.Errorf("DOCX XML exceeds 200000 tokens")
		}
		switch node := token.(type) {
		case xml.StartElement:
			if len(stack) == 0 {
				roots++
				if roots > 1 {
					return fmt.Errorf("DOCX XML has multiple roots")
				}
			}
			if len(stack) >= 128 || len(node.Attr) > 64 {
				return fmt.Errorf("DOCX XML exceeds depth or attribute budgets")
			}
			seen := map[xml.Name]bool{}
			for _, attr := range node.Attr {
				if seen[attr.Name] || len(attr.Value) > 8192 {
					return fmt.Errorf("DOCX XML has duplicate or oversized attributes")
				}
				seen[attr.Name] = true
			}
			stack = append(stack, node.Name)
		case xml.Directive:
			return fmt.Errorf("DOCX XML directives are unsupported")
		case xml.ProcInst:
			if node.Target != "xml" || declaration || roots != 0 {
				return fmt.Errorf("DOCX XML processing instructions are unsupported")
			}
			declaration = true
		case xml.CharData:
			if len(stack) == 0 && len(bytes.TrimSpace(node)) != 0 {
				return fmt.Errorf("DOCX XML contains text outside its root")
			}
		}
		if err := visit(token, stack); err != nil {
			return err
		}
		if _, ok := token.(xml.EndElement); ok {
			stack = stack[:len(stack)-1]
		}
	}
}

func attribute(node xml.StartElement, name string) string {
	for _, attr := range node.Attr {
		if attr.Name.Space == "" && attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func mainPart(ctx context.Context, relationships []byte) (string, error) {
	main := ""
	ids := map[string]bool{}
	err := scanXML(ctx, relationships, func(token xml.Token, stack []xml.Name) error {
		node, ok := token.(xml.StartElement)
		if !ok {
			return nil
		}
		if len(stack) == 1 && node.Name != (xml.Name{Space: relsNS, Local: "Relationships"}) {
			return fmt.Errorf("DOCX package relationship root is invalid")
		}
		if len(stack) == 2 && node.Name == (xml.Name{Space: relsNS, Local: "Relationship"}) {
			id, kind := attribute(node, "Id"), attribute(node, "Type")
			if id == "" || ids[id] || kind == "" || attribute(node, "Target") == "" {
				return fmt.Errorf("DOCX relationship identity is invalid or duplicated")
			}
			ids[id] = true
			if kind != "http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" && kind != "http://purl.oclc.org/ooxml/officeDocument/relationships/officeDocument" {
				return nil
			}
			if main != "" || (attribute(node, "TargetMode") != "" && attribute(node, "TargetMode") != "Internal") {
				return fmt.Errorf("DOCX must have one internal main document relationship")
			}
			var err error
			main, err = partTarget(attribute(node, "Target"))
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if main == "" {
		return "", fmt.Errorf("DOCX main document relationship is missing")
	}
	return main, nil
}

func checkContentType(ctx context.Context, data []byte, main string) error {
	types, defaults := map[string]string{}, map[string]string{}
	err := scanXML(ctx, data, func(token xml.Token, stack []xml.Name) error {
		node, ok := token.(xml.StartElement)
		if !ok {
			return nil
		}
		if len(stack) == 1 && node.Name != (xml.Name{Space: typesNS, Local: "Types"}) {
			return fmt.Errorf("DOCX content-type root is invalid")
		}
		if len(stack) != 2 || node.Name.Space != typesNS {
			return nil
		}
		switch node.Name.Local {
		case "Override":
			part, err := partTarget(attribute(node, "PartName"))
			_, exists := types[part]
			if err != nil || exists || attribute(node, "ContentType") == "" {
				return fmt.Errorf("DOCX content-type part is invalid or duplicated")
			}
			types[part] = attribute(node, "ContentType")
		case "Default":
			ext := attribute(node, "Extension")
			_, exists := defaults[ext]
			if ext == "" || exists || attribute(node, "ContentType") == "" {
				return fmt.Errorf("DOCX default content type is invalid or duplicated")
			}
			defaults[ext] = attribute(node, "ContentType")
		}
		return nil
	})
	if err != nil {
		return err
	}
	kind := types[main]
	if kind == "" {
		kind = defaults[strings.TrimPrefix(path.Ext(main), ".")]
	}
	if kind != mainType {
		return fmt.Errorf("DOCX main part is not a non-macro Word document")
	}
	return nil
}

func bodyText(ctx context.Context, data []byte) (string, error) {
	var text strings.Builder
	body, paragraphs, runs, skip, fields := 0, 0, 0, 0, 0
	bodies := 0
	namespace := ""
	appendText := func(value string) error {
		if text.Len()+len(value) > MaxTextBytes {
			return fmt.Errorf("DOCX extracted text exceeds 128 KiB")
		}
		text.WriteString(value)
		return nil
	}
	err := scanXML(ctx, data, func(token xml.Token, stack []xml.Name) error {
		switch node := token.(type) {
		case xml.StartElement:
			if len(stack) == 1 {
				if node.Name.Local != "document" || (node.Name.Space != wordNS && node.Name.Space != strictWordNS) {
					return fmt.Errorf("DOCX main document root is unsupported")
				}
				namespace = node.Name.Space
			}
			if node.Name == (xml.Name{Space: namespace, Local: "body"}) {
				if len(stack) != 2 || bodies != 0 {
					return fmt.Errorf("DOCX must have one top-level body")
				}
				bodies++
				body = len(stack)
			}
			if body == 0 || node.Name.Space != namespace || skip > 0 {
				return nil
			}
			switch node.Name.Local {
			case "del", "moveFrom", "drawing", "pict", "object", "fldSimple", "altChunk":
				skip = len(stack)
			case "p":
				paragraphs++
			case "r":
				runs++
			case "fldChar":
				for _, attr := range node.Attr {
					if attr.Name == (xml.Name{Space: namespace, Local: "fldCharType"}) {
						if attr.Value == "begin" {
							fields++
						} else if attr.Value == "end" {
							if fields == 0 {
								return fmt.Errorf("DOCX field boundaries are invalid")
							}
							fields--
						}
					}
				}
			case "tab", "br", "cr":
				if paragraphs > 0 && runs > 0 && fields == 0 {
					if node.Name.Local == "tab" {
						return appendText("\t")
					}
					return appendText("\n")
				}
			}
		case xml.CharData:
			if body > 0 && skip == 0 && paragraphs > 0 && runs > 0 && fields == 0 && len(stack) > 0 && stack[len(stack)-1] == (xml.Name{Space: namespace, Local: "t"}) {
				return appendText(string(node))
			}
		case xml.EndElement:
			if skip > 0 {
				if len(stack) == skip {
					skip = 0
				}
				return nil
			}
			if body > 0 && node.Name.Space == namespace {
				switch node.Name.Local {
				case "p":
					paragraphs--
					return appendText("\n")
				case "r":
					runs--
				case "body":
					body = 0
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if bodies != 1 || fields != 0 {
		return "", fmt.Errorf("DOCX body or field boundaries are invalid")
	}
	return text.String(), nil
}

// DOCX reads the main WordprocessingML story only. Comments, headers, footers,
// notes, drawings, deleted revisions and field instructions/results are omitted.
func DOCX(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(data) > MaxDOCXBytes {
		return "", fmt.Errorf("DOCX package exceeds 2 MiB")
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(reader.File) > 256 {
		return "", fmt.Errorf("DOCX package is invalid or exceeds 256 entries")
	}
	parts := map[string]*zip.File{}
	seen := map[string]bool{}
	var expanded uint64
	for _, file := range reader.File {
		name := strings.TrimSuffix(file.Name, "/")
		if !validPart(name) || seen[strings.ToLower(name)] || file.Flags&1 != 0 || file.CompressedSize64 > uint64(len(data)) || (file.Method != zip.Store && file.Method != zip.Deflate) || (file.Mode().Type() != 0 && file.Mode().Type() != fs.ModeDir) {
			return "", fmt.Errorf("DOCX contains an invalid, duplicate, encrypted or non-regular entry")
		}
		parts[name] = file
		seen[strings.ToLower(name)] = true
		if file.UncompressedSize64 > 32<<20 || expanded > 32<<20-file.UncompressedSize64 {
			return "", fmt.Errorf("DOCX expanded package exceeds 32 MiB")
		}
		expanded += file.UncompressedSize64
	}
	relationships, err := readPart(parts["_rels/.rels"], 64<<10)
	if err != nil {
		return "", err
	}
	main, err := mainPart(ctx, relationships)
	if err != nil {
		return "", err
	}
	types, err := readPart(parts["[Content_Types].xml"], 64<<10)
	if err != nil {
		return "", err
	}
	if err := checkContentType(ctx, types, main); err != nil {
		return "", err
	}
	content, err := readPart(parts[main], 4<<20)
	if err != nil {
		return "", err
	}
	return bodyText(ctx, content)
}
