// Package extract derives candidate claims from redacted memory text. The
// default extractor is deterministic, offline and rule-based: it recognizes a
// small set of explicit decision, constraint, dependency and reference
// phrasings and skips questioned, hedged or negated statements. Every claim
// carries the exact sentence it was derived from as its quote.
package extract

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RulesVersion identifies the rule set. Bump it whenever extraction output can
// change so stored memories are re-extracted and obsolete claims retracted.
const RulesVersion = "rules-v1"

const (
	// MaxClaims bounds the claims taken from one memory.
	MaxClaims = 16
	// MaxReferenceClaims bounds the file/symbol reference claims of one memory.
	MaxReferenceClaims = 8
	// MaxQuoteBytes bounds the evidence quote stored with each claim.
	MaxQuoteBytes = 512
	maxValueBytes = 120
	maxRoleTokens = 4
)

// Predicates of the extraction schema. uses/stopped_using and
// depends_on/dropped_dependency are negation pairs for automatic invalidation.
const (
	Uses              = "uses"
	StoppedUsing      = "stopped_using"
	Replaces          = "replaces"
	DependsOn         = "depends_on"
	DroppedDependency = "dropped_dependency"
	Policy            = "policy"
	Prefers           = "prefers"
	DefinedIn         = "defined_in"
	ReferencedIn      = "referenced_in"
)

// ProjectSubject is the subject of project-wide decisions without a role.
const ProjectSubject = "project"

// Claim is one candidate. Quote is an exact substring of the extracted text.
type Claim struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	Exclusive bool   `json:"exclusive"`
	Quote     string `json:"quote"`
}

// Input is redacted memory text. Reference names the source entity used as
// the object of reference claims, e.g. "turn:ID" or "memory:ID".
type Input struct {
	Text      string
	Reference string
	// TurnFormat marks Trace turn memories ("Turn ID (...): prompt |
	// reasoning: ..."); their identity prefix and Thinking blocks are skipped.
	TurnFormat bool
}

// Exclusive reports whether a subject/predicate pair holds one value at a time.
// Both extractors use it so their claims invalidate each other consistently.
func Exclusive(subject, predicate string) bool {
	switch predicate {
	case Uses:
		return subject != ProjectSubject
	case Policy, DefinedIn:
		return true
	case Prefers:
		return strings.Contains(subject, " vs ")
	}
	return false
}

// negations pairs predicates whose claims about the same object contradict.
var negations = map[string]string{Uses: StoppedUsing, StoppedUsing: Uses, DependsOn: DroppedDependency, DroppedDependency: DependsOn}

// Negation returns the predicate that contradicts predicate for the same
// subject and object, or "" when there is none.
func Negation(predicate string) string { return negations[predicate] }

// NegationPairs lists each negation pair once, in a stable order.
func NegationPairs() [][2]string {
	return [][2]string{{Uses, StoppedUsing}, {DependsOn, DroppedDependency}}
}

// KnownPredicate reports whether predicate belongs to the extraction schema.
func KnownPredicate(predicate string) bool {
	switch predicate {
	case Uses, StoppedUsing, Replaces, DependsOn, DroppedDependency, Policy, Prefers, DefinedIn, ReferencedIn:
		return true
	}
	return false
}

// Rules extracts claims in text order within the per-memory budget. For an
// exclusive subject/predicate pair only the memory's last statement is kept,
// so one memory never contradicts itself.
func Rules(in Input) []Claim {
	if !utf8.ValidString(in.Text) {
		return nil
	}
	decisions, references := []Claim{}, []Claim{}
	seenRefs := map[string]bool{}
	for _, s := range sentences(in.Text, in.TurnFormat) {
		decisions = append(decisions, decide(s)...)
		if in.Reference == "" {
			continue
		}
		for _, name := range referencesIn(s) {
			if !seenRefs[name] {
				seenRefs[name] = true
				references = append(references, Claim{Subject: name, Predicate: ReferencedIn, Object: in.Reference, Quote: s})
			}
		}
	}
	claims := Dedupe(decisions)
	if len(claims) > MaxClaims {
		claims = claims[:MaxClaims]
	}
	if len(references) > MaxReferenceClaims {
		references = references[:MaxReferenceClaims]
	}
	if room := MaxClaims - len(claims); len(references) > room {
		references = references[:room]
	}
	return append(claims, references...)
}

// Dedupe drops repeated triples and keeps, within one memory, only the last
// statement of each exclusive subject/predicate pair and of each negation pair
// about the same subject and value, so "npm install x … npm uninstall x" or
// "we use X … we no longer use X" yield only the later statement.
func Dedupe(claims []Claim) []Claim {
	groups := func(c Claim) []string {
		keys := []string{}
		if c.Exclusive {
			keys = append(keys, "exclusive\x00"+c.Subject+"\x00"+c.Predicate)
		}
		if negation := Negation(c.Predicate); negation != "" {
			pair := []string{c.Predicate, negation}
			sort.Strings(pair)
			keys = append(keys, "negation\x00"+c.Subject+"\x00"+c.Object+"\x00"+pair[0])
		}
		return keys
	}
	last := map[string]int{}
	for i, c := range claims {
		for _, key := range groups(c) {
			last[key] = i
		}
	}
	seen := map[string]bool{}
	out := []Claim{}
	for i, c := range claims {
		superseded := false
		for _, key := range groups(c) {
			superseded = superseded || last[key] != i
		}
		key := c.Subject + "\x00" + c.Predicate + "\x00" + c.Object
		if superseded || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

var turnPrefix = regexp.MustCompile(`^Turn [^\s()]+ \(session [^\s()]+, seq -?\d+\): `)

// responseLabels mark recorded responses and outputs in Trace turn reasoning.
// Trace stores some agents' private thinking unlabelled and others' under a
// "Thinking: " label, and joins every note with a blank line, so only a
// paragraph that starts with one of these labels is treated as a statement.
var responseLabels = []string{"Assistant response: ", "Agent output: "}

// sentences returns trimmed sentences; each is an exact substring of text.
func sentences(text string, turnFormat bool) []string {
	blocks := []string{text}
	if turnFormat {
		blocks = turnBlocks(text)
	}
	out := []string{}
	for _, block := range blocks {
		start := 0
		for i := 0; i < len(block); i++ {
			end := -1
			switch c := block[i]; {
			case c == '\n':
				end = i
			case (c == '.' || c == '!' || c == '?' || c == ';') && (i+1 == len(block) || block[i+1] == ' ' || block[i+1] == '\n' || block[i+1] == '\t'):
				end = i + 1
			case c == '|' && i > 0 && block[i-1] == ' ' && i+1 < len(block) && block[i+1] == ' ':
				end = i
			}
			if end < 0 {
				continue
			}
			if s := cleanSentence(block[start:end]); s != "" {
				out = append(out, s)
			}
			start = i + 1
		}
		if s := cleanSentence(block[start:]); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func turnBlocks(text string) []string {
	if loc := turnPrefix.FindStringIndex(text); loc != nil {
		text = text[loc[1]:]
	}
	prompt, reasoning, found := strings.Cut(text, " | reasoning: ")
	blocks := []string{prompt}
	if !found {
		return blocks
	}
	for _, paragraph := range strings.Split(reasoning, "\n\n") {
		for _, label := range responseLabels {
			if strings.HasPrefix(paragraph, label) {
				blocks = append(blocks, paragraph[len(label):])
			}
		}
	}
	return blocks
}

func cleanSentence(s string) string {
	s = strings.TrimSpace(s)
	for _, prefix := range []string{"- ", "* ", "• ", "> ", "+ "} {
		s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
	}
	if len(s) < 4 || len(s) > MaxQuoteBytes {
		return ""
	}
	return s
}

// Hedges anywhere in a sentence, and conditionals/modals before the matched
// statement, mark it as hypothetical rather than a recorded decision.
var anywhereHedges = regexp.MustCompile(`(?i)\b(?:maybe|perhaps|whether|wonder|unsure|not sure|consider|considering|propose|proposing|suggest|suggesting|should we|shall we|do we|can we|plan to|planning to|want to|wants to|thinking about|hypothetically|for example|e\.g|if|unless|when|whenever|in case|provided that)\b`)
var prefixHedges = regexp.MustCompile(`(?i)\b(?:could|would|might|may|should|try|trying)\b`)
var negated = regexp.MustCompile(`(?i)(?:\bnot|n't|\bnever|\bno)\s+(?:\w+\s+)?$`)

func hedged(s string, offset int) bool {
	return strings.HasSuffix(s, "?") || anywhereHedges.MatchString(s) || prefixHedges.MatchString(s[:offset])
}

const symbolPattern = "`[^`\\s]+`|[A-Za-z_][\\w]*(?:\\.[A-Za-z_]\\w*)*(?:\\(\\))?"

var (
	switchRule  = regexp.MustCompile(`(?i)\b(?:switched|migrated|transitioned)\s+(?:(?:the|our)\s+)?(?:([a-z][\w-]*(?:\s+[a-z][\w-]*){0,3})\s+)?from\s+(.+?)\s+(?:over\s+to|to|onto)\s+(.+)`)
	replaceRule = regexp.MustCompile(`(?i)\b(?:we|team)\s+(?:have\s+|'ve\s+)?replaced\s+(.+?)\s+with\s+(.+)`)
	useRule     = regexp.MustCompile(`(?i)\b(?:we|team|the project|this project)\s+(?:now\s+|currently\s+|still\s+|will\s+)?(?:use|uses|are using|'re using|rely on|adopted|have adopted|standardized on|standardised on|chose|picked|selected|went with|decided to use|decided on|settled on|switched to|migrated to)\s+(.+)`)
	imperative  = regexp.MustCompile(`(?i)^(?:please\s+)?use\s+(.+?)\s+for\s+(.+)`)
	ourRule     = regexp.MustCompile(`(?i)(?:^|\s)([\w.+#@/-]+)\s+is\s+(?:now\s+)?our\s+([a-z][\w-]*(?:\s+[a-z][\w-]*){0,3})`)
	stopRule    = regexp.MustCompile(`(?i)\b(?:no longer use|no longer uses|no longer rely on|stopped using)\s+(.+)`)
	policyRule  = regexp.MustCompile(`(?i)(?:^|\b(?:we|you|must|should|please)\s+)(always|never)\s+(.+)`)
	forbidRule  = regexp.MustCompile(`(?i)^(?:please\s+)?(?:do not|don't|must not|mustn't)\s+(.+)`)
	preferRule  = regexp.MustCompile(`(?i)\bprefers?\s+(.+?)\s+(?:over|rather than|instead of)\s+(.+)`)
	addDepRule  = regexp.MustCompile(`(?i)\b(?:added|add|introduced)\s+(?:(?:the|a|an)\s+)?(?:new\s+)?(?:dependency|package|module|library)\s+(?:on\s+)?(.+)`)
	asDepRule   = regexp.MustCompile(`(?i)\b(?:added|add)\s+(.+?)\s+as\s+(?:a\s+)?(?:new\s+)?(?:dependency|dep)\b`)
	dropDepRule = regexp.MustCompile(`(?i)\b(?:removed|remove|dropped|drop)\s+(?:(?:the|a|an)\s+)?(?:dependency|package|module|library)\s+(?:on\s+)?(.+)`)
	installRule = regexp.MustCompile("(?:^|[\\s$>`(])(go get|npm install|npm i|yarn add|pnpm add|pip install|pip3 install|cargo add|poetry add|bundle add|gem install|composer require)\\s+(.+)")
	removeRule  = regexp.MustCompile("(?:^|[\\s$>`(])(npm uninstall|npm remove|npm rm|yarn remove|pnpm remove|pip uninstall|pip3 uninstall|cargo remove|poetry remove|bundle remove|composer remove)\\s+(.+)")
	dependsRule = regexp.MustCompile(`(?i)(?:^|\s)([\w.@/-]+)\s+(?:now\s+)?depends\s+on\s+(.+)`)
	definedRule = regexp.MustCompile(`(?i)(` + symbolPattern + `)\s+(?:is\s+now|now\s+lives|lives|is)\s+(?:defined\s+|declared\s+|implemented\s+)?in\s+(\S+)`)
	movedRule   = regexp.MustCompile(`(?i)\bmoved\s+(` + symbolPattern + `)\s+(?:from\s+\S+\s+)?(?:to|into)\s+(\S+)`)
)

// ruleHints are lowercase fragments a sentence must contain before a rule's
// regular expression runs; they only skip work and never cause a match.
var ruleHints = map[*regexp.Regexp][]string{
	switchRule: {"switched", "migrated", "transitioned"}, replaceRule: {"replaced"}, useRule: {"we ", "team ", "project "},
	imperative: {"use"}, ourRule: {" our "}, stopRule: {"no longer", "stopped using"}, policyRule: {"always", "never"}, forbidRule: {"not ", "n't "},
	preferRule: {"prefer"}, addDepRule: {"add", "introduced"}, asDepRule: {" as "}, dropDepRule: {"remove", "drop"}, installRule: {"go get", "install", " add ", "require"},
	removeRule: {"uninstall", "remove", " rm "}, dependsRule: {"depends on"}, definedRule: {"is in", "is now", "lives in", "defined in", "declared in", "implemented in"}, movedRule: {"moved"},
}

// Imperatives such as "don't worry" are conversational, not project policy.
var conversationalVerbs = map[string]bool{"worry": true, "forget": true, "hesitate": true, "mind": true, "bother": true, "panic": true, "be": true, "know": true, "think": true, "see": true, "understand": true, "need": true, "have": true, "get": true, "feel": true}

// decide applies the decision, constraint and dependency rules to s.
func decide(s string) []Claim {
	claims := []Claim{}
	add := func(subject, predicate, object string) {
		if subject == "" || object == "" || subject == object {
			return
		}
		claims = append(claims, Claim{Subject: subject, Predicate: predicate, Object: object, Exclusive: Exclusive(subject, predicate), Quote: s})
	}
	lower := strings.ToLower(s)
	match := func(rule *regexp.Regexp) []int {
		if hint := ruleHints[rule]; hint != nil && !slices.ContainsFunc(hint, func(h string) bool { return strings.Contains(lower, h) }) {
			return nil
		}
		m := rule.FindStringSubmatchIndex(s)
		if m == nil || hedged(s, m[0]) || negated.MatchString(s[:m[0]]) {
			return nil
		}
		return m
	}
	group := func(m []int, i int) string {
		if m[2*i] < 0 {
			return ""
		}
		return s[m[2*i]:m[2*i+1]]
	}
	decided := false
	for _, rule := range []*regexp.Regexp{definedRule, movedRule} {
		if m := match(rule); m != nil {
			if symbol, file := symbolName(group(m, 1)), filePath(group(m, 2)); symbol != "" && file != "" {
				add(symbol, DefinedIn, file)
				decided = true
				break
			}
		}
	}
	if m := match(switchRule); !decided && m != nil {
		from, _ := value(group(m, 2), 3)
		to, rest := value(group(m, 3), 3)
		role := roleName(group(m, 1))
		if role == "" {
			role = roleAfterFor(rest)
		}
		decided = switchClaims(add, role, from, to)
	}
	if m := match(replaceRule); !decided && m != nil {
		from, _ := value(group(m, 1), 3)
		to, rest := value(group(m, 2), 3)
		decided = switchClaims(add, roleAfterFor(rest), from, to)
	}
	if m := match(imperative); !decided && m != nil {
		if tool, _ := value(group(m, 1), 3); tool != "" {
			if role := roleName(group(m, 2)); role != "" {
				add(role, Uses, strings.ToLower(tool))
				decided = true
			}
		}
	}
	if m := match(useRule); !decided && m != nil {
		tool, rest := value(group(m, 1), 3)
		if role := roleAfterFor(rest); role != "" {
			add(role, Uses, strings.ToLower(tool))
			decided = tool != ""
		} else if toolLike(tool) {
			add(ProjectSubject, Uses, strings.ToLower(tool))
			decided = true
		}
	}
	if m := match(ourRule); !decided && m != nil {
		if tool, _ := value(group(m, 1), 2); toolLike(tool) {
			add(roleName(group(m, 2)), Uses, strings.ToLower(tool))
		}
	}
	if m := match(stopRule); m != nil {
		tool, rest := value(group(m, 1), 3)
		role := roleAfterFor(rest)
		if role == "" {
			role = ProjectSubject
		}
		add(role, StoppedUsing, strings.ToLower(tool))
	}
	if m := match(preferRule); m != nil {
		preferred, _ := value(group(m, 1), 3)
		other, _ := value(group(m, 2), 3)
		preferred, other = strings.ToLower(preferred), strings.ToLower(other)
		if preferred != "" && other != "" && preferred != other {
			pair := []string{preferred, other}
			sort.Strings(pair)
			add(pair[0]+" vs "+pair[1], Prefers, preferred)
		}
	}
	if m := match(policyRule); m != nil {
		add(action(group(m, 2)), Policy, strings.ToLower(group(m, 1)))
	} else if m := match(forbidRule); m != nil {
		add(action(group(m, 1)), Policy, "never")
	}
	for _, rule := range []*regexp.Regexp{addDepRule, asDepRule} {
		if m := match(rule); m != nil {
			if name, _ := value(group(m, 1), 1); name != "" {
				add(ProjectSubject, DependsOn, packageName(strings.ToLower(name)))
			}
			break
		}
	}
	if m := match(dropDepRule); m != nil {
		if name, _ := value(group(m, 1), 1); name != "" {
			add(ProjectSubject, DroppedDependency, packageName(strings.ToLower(name)))
		}
	}
	for _, rule := range []struct {
		rx        *regexp.Regexp
		predicate string
	}{{installRule, DependsOn}, {removeRule, DroppedDependency}} {
		if m := match(rule.rx); m != nil {
			for _, name := range commandPackages(group(m, 2)) {
				add(ProjectSubject, rule.predicate, name)
			}
		}
	}
	if m := match(dependsRule); m != nil {
		subject, _ := trimToken(group(m, 1))
		if strings.EqualFold(subject, ProjectSubject) {
			subject = ProjectSubject
		} else if !nameLike(subject) {
			subject = ""
		}
		if target, _ := value(group(m, 2), 3); target != "" && subject != "" {
			add(subject, DependsOn, strings.ToLower(target))
		}
	}
	return claims
}

// switchClaims records the new value, the explicit end of the old value and
// the replacement relation.
func switchClaims(add func(string, string, string), role, from, to string) bool {
	from, to = strings.ToLower(from), strings.ToLower(to)
	if from == "" || to == "" || from == to {
		return false
	}
	if role == "" {
		role = ProjectSubject
	}
	add(role, Uses, to)
	add(role, StoppedUsing, from)
	add(to, Replaces, from)
	return true
}

var stopwords, determiners, pronouns = map[string]bool{}, map[string]bool{}, map[string]bool{}

func init() {
	for _, w := range strings.Fields("to for with because since as in on of and or but so instead now from when while which that then after before at by via using over than if until unless into onto across per like where whereas though although except plus also too is are was were be been being will would can could should must may might do does did not") {
		stopwords[w] = true
	}
	for _, w := range strings.Fields("the a an our their its this that these those my your some any lot lots many several few more less much bunch kind sort") {
		determiners[w] = true
	}
	for _, w := range strings.Fields("it this that them these those something anything everything nothing one which what they we you i he she him her there here both either neither all none same other others itself whatever whichever") {
		pronouns[w] = true
	}
}

// trimToken strips surrounding punctuation, keeping inner dots and slashes
// and the characters of names such as C#, C++ or @scope/package.
func trimToken(token string) (string, bool) {
	core := strings.TrimRightFunc(token, func(r rune) bool {
		return (unicode.IsPunct(r) || unicode.IsSymbol(r)) && r != '#' && r != '+' && r != '_'
	})
	terminal := core != token
	core = strings.TrimLeftFunc(core, func(r rune) bool {
		return (unicode.IsPunct(r) || unicode.IsSymbol(r)) && r != '@' && r != '.' && r != '_'
	})
	return core, terminal
}

// value reads up to max name tokens, skipping leading determiners and stopping
// at a stopword, a parenthetical or terminal punctuation. rest starts after
// the consumed tokens.
func value(span string, max int) (string, string) {
	fields := strings.Fields(span)
	tokens := []string{}
	consumed := 0
	for _, field := range fields {
		core, terminal := trimToken(field)
		lower := strings.ToLower(core)
		if core == "" || (strings.HasPrefix(field, "(") && len(tokens) > 0) {
			break
		}
		if len(tokens) == 0 && determiners[lower] {
			consumed++
			continue
		}
		if stopwords[lower] {
			break
		}
		tokens = append(tokens, core)
		consumed++
		if terminal || len(tokens) == max {
			break
		}
	}
	result := strings.Join(tokens, " ")
	if !validValue(strings.ToLower(result)) {
		result = ""
	}
	return result, strings.Join(fields[consumed:], " ")
}

// roleName parses the purpose of a decision, e.g. "the test runner".
func roleName(span string) string {
	role, _ := value(span, maxRoleTokens)
	role = strings.ToLower(role)
	if role == ProjectSubject {
		return ""
	}
	return role
}

// roleAfterFor reads "for ROLE" directly after a value, allowing one
// parenthetical aside such as "SQLite (pure Go) for storage".
func roleAfterFor(rest string) string {
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "(") {
		if end := strings.Index(rest, ")"); end >= 0 {
			rest = strings.TrimSpace(rest[end+1:])
		}
	}
	if len(rest) < 4 || !strings.EqualFold(rest[:4], "for ") {
		return ""
	}
	return roleName(rest[4:])
}

func validValue(v string) bool {
	if v == "" || len(v) > maxValueBytes || strings.Contains(v, "⟨") || strings.Contains(v, "redacted") {
		return false
	}
	if pronouns[v] || stopwords[v] || determiners[v] {
		return false
	}
	return strings.ContainsFunc(v, unicode.IsLetter)
}

// toolLike accepts a project-wide "we use X" only when X is written like a
// name (capitals, digits or name punctuation), not like an ordinary noun.
func toolLike(v string) bool {
	return v != "" && strings.ContainsFunc(v, func(r rune) bool {
		return unicode.IsUpper(r) || unicode.IsDigit(r) || strings.ContainsRune(".-/+#@_", r)
	})
}

// nameLike accepts the subject of "X depends on Y" only when it names code or
// a component: a path, an identifier, a qualified name or an acronym.
func nameLike(v string) bool {
	if len(v) < 2 || filePath(v) != "" || symbolName(v) != "" || strings.ContainsAny(v, "./@") {
		return v != ""
	}
	return strings.ToUpper(v) == v && strings.ContainsFunc(v, unicode.IsLetter)
}

var actionPrefixes = []string{"make sure to ", "make sure you ", "be sure to ", "remember to ", "to "}
var actionStops = map[string]bool{"because": true, "since": true, "unless": true, "except": true, "otherwise": true, "but": true}

// action normalizes the constrained behaviour of a policy: 2–12 lowercase words.
func action(span string) string {
	lower := strings.ToLower(span)
	for _, prefix := range actionPrefixes {
		lower = strings.TrimPrefix(lower, prefix)
	}
	words := []string{}
	for _, field := range strings.Fields(lower) {
		core, terminal := trimToken(field)
		if core == "" || actionStops[core] {
			break
		}
		words = append(words, core)
		if terminal || len(words) == 12 {
			break
		}
	}
	result := strings.Join(words, " ")
	if len(words) < 2 || conversationalVerbs[words[0]] || pastTense(words[0]) || len(result) > 256 || strings.Contains(result, "⟨") {
		return ""
	}
	return result
}

var irregularPast = map[string]bool{"was": true, "were": true, "had": true, "did": true, "went": true, "made": true, "got": true, "ran": true, "wrote": true, "took": true, "saw": true, "knew": true, "found": true, "said": true, "told": true, "came": true}
var edVerbs = map[string]bool{"embed": true, "proceed": true, "exceed": true, "succeed": true, "speed": true, "breed": true, "bleed": true, "shred": true, "need": true, "seed": true, "feed": true, "shed": true, "heed": true, "weed": true}

// pastTense marks narrative ("we never migrated ...") rather than a rule
// ("never migrate ...").
func pastTense(word string) bool {
	return irregularPast[word] || (len(word) >= 4 && strings.HasSuffix(word, "ed") && !edVerbs[word])
}

var versionSuffix = regexp.MustCompile(`(?:@[^/@\s]+|[=<>~!]=.*)$`)

// packageName drops a version pin so an upgrade is not a new dependency.
func packageName(name string) string {
	if trimmed := versionSuffix.ReplaceAllString(name, ""); trimmed != "" && trimmed != "@" {
		return trimmed
	}
	return name
}

// commandPackages reads package arguments of a package-manager command,
// skipping flags (and the values of -r/-c/-e) and stopping at shell operators.
func commandPackages(args string) []string {
	out := []string{}
	skip := false
	for _, field := range strings.Fields(args) {
		if strings.ContainsAny(field[:1], "|&;><") {
			break
		}
		if skip {
			skip = false
			continue
		}
		if strings.HasPrefix(field, "-") {
			skip = field == "-r" || field == "-c" || field == "-e" || field == "--requirement" || field == "--constraint" || field == "--editable"
			continue
		}
		core, terminal := trimToken(field)
		name := strings.ToLower(packageName(core))
		if validValue(name) && filePath(name) == "" && !strings.ContainsAny(name, "*$") {
			out = append(out, name)
		}
		if terminal || len(out) == 4 {
			break
		}
	}
	return out
}

var fileExtensions, bareTools = map[string]bool{}, map[string]bool{}
var namedFiles = map[string]bool{"Makefile": true, "Dockerfile": true, "Gemfile": true, "Rakefile": true, "Procfile": true, "Justfile": true}

func init() {
	for _, ext := range strings.Fields("go mod sum ts tsx js jsx mjs cjs json py rb rs java kt kts swift c h cc cpp hpp cs php sql sh bash zsh fish yml yaml toml ini cfg conf md txt rst html css scss less vue svelte proto graphql gql tf lock xml gradle") {
		fileExtensions[ext] = true
	}
	for _, tool := range strings.Fields("node.js next.js vue.js nuxt.js express.js react.js angular.js ember.js three.js d3.js chart.js moment.js nest.js svelte.js solid.js alpine.js deno.js bun.js ext.js p5.js") {
		bareTools[tool] = true
	}
}

// filePath validates a repository-relative path with a known extension.
func filePath(token string) string {
	core, _ := trimToken(token)
	core = strings.TrimPrefix(core, "./")
	if core == "" || len(core) > 256 || strings.Contains(core, "://") || strings.HasPrefix(core, "/") || strings.Contains(core, "..") || strings.Contains(core, "⟨") {
		return ""
	}
	base := core[strings.LastIndex(core, "/")+1:]
	if namedFiles[base] {
		return core
	}
	dot := strings.LastIndex(base, ".")
	if dot <= 0 || !fileExtensions[strings.ToLower(base[dot+1:])] || (!strings.Contains(core, "/") && bareTools[strings.ToLower(core)]) {
		return ""
	}
	for _, r := range core {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/._-@+", r)) {
			return ""
		}
	}
	return core
}

var symbolToken = regexp.MustCompile("`([A-Za-z_][\\w]*(?:(?:\\.|::)[A-Za-z_]\\w*)*)(?:\\(\\))?`|\\b(?:func|function|def|class|type|interface|struct)\\s+([A-Za-z_]\\w*(?:\\.[A-Za-z_]\\w*)?)")

// symbolName validates a code identifier: backticked, or shaped like code
// (inner capitals, underscore, qualification or call parentheses) rather than
// an English word.
func symbolName(token string) string {
	backticked := len(token) > 2 && strings.HasPrefix(token, "`") && strings.HasSuffix(token, "`")
	called := strings.HasSuffix(strings.Trim(token, "`"), "()")
	core := strings.TrimSuffix(strings.Trim(token, "`"), "()")
	lower := strings.ToLower(core)
	if len(core) < 2 || len(core) > 128 || filePath(core) != "" || stopwords[lower] || pronouns[lower] || determiners[lower] {
		return ""
	}
	codeShaped := backticked || called || strings.ContainsAny(core, "_.:")
	for i, r := range core {
		if !(r == '_' || r == '.' || r == ':' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r))) {
			return ""
		}
		if i > 0 && unicode.IsUpper(r) {
			codeShaped = true
		}
	}
	if !codeShaped {
		return ""
	}
	return core
}

// referencesIn lists file paths, then code symbols, mentioned in s.
func referencesIn(s string) []string {
	out := []string{}
	for _, token := range strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("\"'()[]<>{},=`|", r)
	}) {
		if path := filePath(token); path != "" {
			out = append(out, path)
		}
	}
	for _, m := range symbolToken.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if symbol := symbolName("`" + name + "`"); len(symbol) >= 3 {
			out = append(out, symbol)
		}
	}
	return out
}
