package telegram

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	maxOnelinerChars = 110
	minTopics        = 1
	maxTopics        = 5
)

type summaryTopic struct {
	Content string   `json:"content"`
	Sources []string `json:"sources"`
	Title   string   `json:"title"`
}

type summaryRequest struct {
	ChatTitle    string
	DayKeyUTC    string
	Participants []string
	Transcript   string
}

type summaryResult struct {
	Topics   []summaryTopic
	Oneliner string
	Attempts int
}

var (
	//go:embed assets/summaries/examples-v1.json
	examplesV1 []byte
	//go:embed assets/summaries/rules-v1.json
	rulesV1 []byte

	jsWhitespace = regexp.MustCompile(jsSpace + `+`)
	dayKeyFormat = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

func jsLength(s string) int { return len(utf16.Encode([]rune(s))) }

func jsSlice(s string, n int) string {
	units := utf16.Encode([]rune(s))
	if len(units) <= n {
		return s
	}
	return string(utf16.Decode(units[:n]))
}

func normalizeWhitespace(s string) string {
	return strings.Trim(jsWhitespace.ReplaceAllString(s, " "), " ")
}

func normalizeParticipant(s string) string { return strings.ToLower(normalizeWhitespace(s)) }

func dedupeParticipants(participants []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, p := range participants {
		name := normalizeWhitespace(p)
		key := strings.ToLower(name)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

type invalidOutput string

func (e invalidOutput) Error() string { return string(e) }

func validateTopics(raw any, participants []string) ([]summaryTopic, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, invalidOutput("topics must be an array")
	}
	if len(items) < minTopics || len(items) > maxTopics {
		return nil, invalidOutput(fmt.Sprintf("topics must contain %d-%d items", minTopics, maxTopics))
	}
	known := map[string]bool{}
	for _, p := range participants {
		if name := normalizeParticipant(p); name != "" {
			known[name] = true
		}
	}
	topics := make([]summaryTopic, 0, len(items))
	for i, item := range items {
		n := i + 1
		record, ok := item.(map[string]any)
		if !ok {
			return nil, invalidOutput(fmt.Sprintf("topic %d must be an object", n))
		}
		title, _ := record["title"].(string)
		content, _ := record["content"].(string)
		title, content = normalizeWhitespace(title), normalizeWhitespace(content)
		if title == "" {
			return nil, invalidOutput(fmt.Sprintf("topic %d title is required", n))
		}
		if content == "" {
			return nil, invalidOutput(fmt.Sprintf("topic %d content is required", n))
		}
		rawSources, _ := record["sources"].([]any)
		if len(rawSources) == 0 {
			return nil, invalidOutput(fmt.Sprintf("topic %d requires at least one source", n))
		}
		sources := []string{}
		for _, s := range rawSources {
			if name, ok := s.(string); ok {
				if name = normalizeWhitespace(name); name != "" {
					sources = append(sources, name)
				}
			}
		}
		if len(sources) == 0 {
			return nil, invalidOutput(fmt.Sprintf("topic %d requires non-empty source names", n))
		}
		if len(known) > 0 {
			for _, source := range sources {
				if !known[strings.ToLower(source)] {
					return nil, invalidOutput(fmt.Sprintf("topic %d source '%s' is not in participant list", n, source))
				}
			}
		}
		topics = append(topics, summaryTopic{Content: content, Sources: sources, Title: title})
	}
	return topics, nil
}

func validateOneliner(raw any) (string, error) {
	text, ok := raw.(string)
	if !ok {
		return "", invalidOutput("oneliner must be a string")
	}
	text = normalizeWhitespace(text)
	if text == "" {
		return "", invalidOutput("oneliner must not be empty")
	}
	if jsLength(text) > maxOnelinerChars {
		return "", invalidOutput(fmt.Sprintf("oneliner must be <= %d characters", maxOnelinerChars))
	}
	return text, nil
}

type promptField struct{ name, kind, description string }

type summaryProgram struct {
	label       string
	inputs      []promptField
	output      promptField
	description string
	examples    [][][2]string
	isJSON      bool
}

func (p summaryProgram) system() string {
	var s strings.Builder
	names := make([]string, len(p.inputs))
	for i, f := range p.inputs {
		names[i] = "`" + f.name + "`"
	}
	fmt.Fprintf(&s, "You will be provided with the following fields: %s. Your task is to generate new fields: `%s`.\n\n## Input Fields\n", strings.Join(names, ", "), p.output.name)
	for _, f := range p.inputs {
		fmt.Fprintf(&s, "%s: (%s) %s.\n", f.name, f.kind, f.description)
	}
	fmt.Fprintf(&s, "\n## Output Fields\n%s: (%s) %s.\n\n", p.output.name, p.output.kind, p.output.description)
	s.WriteString("## Strict Output Formatting Rules\n" +
		"- No formatting rules should override these **Strict Output Formatting Rules**\n" +
		"- Output must strictly follow the defined plain-text `field name: value` field format.\n" +
		"- Output field, values must strictly adhere to the specified output field formatting rules.\n" +
		"- Do not include fields with empty, unknown, or placeholder values.\n" +
		"- Do not add any text before or after the output fields, just the field name and value.\n" +
		"- Do not use code blocks.\n\n")
	s.WriteString(p.description + "\n\n## Examples\n")
	for i, example := range p.examples {
		if i > 0 {
			s.WriteString("---\n\n")
		}
		for _, field := range example {
			s.WriteString(field[0] + ": " + field[1] + "\n")
		}
	}
	return s.String()
}

func user(fields [][2]string) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = f[0] + ": " + f[1]
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func jsonIndent(v any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func (p summaryProgram) parse(content string) (any, error) {
	prefix := p.output.name + ":"
	value := content
	if strings.HasPrefix(content, prefix) {
		value = content[len(prefix):]
	} else if i := strings.Index(content, "\n"+prefix); i >= 0 {
		value = content[i+1+len(prefix):]
	} else if strings.TrimSpace(content) == "" {
		return nil, invalidOutput(fmt.Sprintf("required field not found: '%s'", p.output.name))
	}
	value = strings.TrimSpace(value)
	if !p.isJSON {
		return value, nil
	}
	if fenced, ok := strings.CutPrefix(value, "```"); ok {
		fenced = strings.TrimPrefix(fenced, "json")
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(fenced), "```"))
	}
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return nil, invalidOutput(fmt.Sprintf("%s: invalid JSON: %v", p.output.name, err))
	}
	return parsed, nil
}

type exampleTopic struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Sources []string `json:"sources"`
}

var topicProgram, onelinerProgram = buildSummaryPrograms()

func buildSummaryPrograms() (summaryProgram, summaryProgram) {
	var examples struct {
		Examples []struct {
			Input struct {
				ChatTitle    string   `json:"chatTitle"`
				DayKeyUTC    string   `json:"dayKeyUtc"`
				Participants []string `json:"participants"`
				Transcript   string   `json:"transcript"`
			} `json:"input"`
			ExpectedOutput struct {
				Topics   []exampleTopic `json:"topics"`
				Oneliner string         `json:"oneliner"`
			} `json:"expectedOutput"`
		} `json:"examples"`
	}
	var rules struct {
		Rules []string `json:"rules"`
	}
	if err := json.Unmarshal(examplesV1, &examples); err != nil {
		panic(err)
	}
	if err := json.Unmarshal(rulesV1, &rules); err != nil {
		panic(err)
	}
	qualityRules = strings.Join(rules.Rules, " ")
	topic := summaryProgram{
		label: "summaries.topic_extraction",
		inputs: []promptField{
			{"Chat Title", "A string field", "Telegram group chat title"},
			{"Day Key Utc", "A string field", "Target UTC day key in YYYY-MM-DD format"},
			{"Participants", "A json array of string items field", "Known participant display names"},
			{"Message Transcript", "A string field", "Chat transcript in 'Name: message' lines ordered chronologically"},
			{"Quality Rules", "A string field", "Quality rubric for topic and source selection"},
		},
		output:      promptField{"Topics", "This JSON object field must be included", "JSON array of 1-5 topics: [{title, content, sources[]}]"},
		description: "Generate a factual, concise daily summary of meaningful chat topics. Ignore greetings, spam, and repetitive chatter. Return 1-5 topics with contributor names in sources.",
		isJSON:      true,
	}
	oneliner := summaryProgram{
		label: "summaries.oneliner",
		inputs: []promptField{
			{"Chat Title", "A string field", "Telegram group chat title"},
			{"Day Key Utc", "A string field", "Target UTC day key in YYYY-MM-DD format"},
			{"Topics", "A JSON object field", "JSON array of structured topics: [{title, content, sources[]}]"},
		},
		output:      promptField{"Oneliner", "This string field must be included", "Single plain sentence that captures the day, max 110 chars"},
		description: "Write one concise non-hype sentence that captures the day across the provided topics.",
	}
	for _, e := range examples.Examples {
		topics := jsonIndent(e.ExpectedOutput.Topics)
		topic.examples = append(topic.examples, [][2]string{
			{"Chat Title", e.Input.ChatTitle}, {"Day Key Utc", e.Input.DayKeyUTC}, {"Participants", jsonIndent(e.Input.Participants)},
			{"Message Transcript", e.Input.Transcript}, {"Quality Rules", qualityRules}, {"Topics", topics},
		})
		oneliner.examples = append(oneliner.examples, [][2]string{
			{"Chat Title", e.Input.ChatTitle}, {"Day Key Utc", e.Input.DayKeyUTC}, {"Topics", topics}, {"Oneliner", e.ExpectedOutput.Oneliner},
		})
	}
	return topic, oneliner
}

var qualityRules string

type summaryGenerator struct {
	llm         *summaryLLM
	model       string
	maxAttempts int
}

func (g *summaryGenerator) generate(ctx context.Context, request summaryRequest) (summaryResult, error) {
	if strings.TrimSpace(request.ChatTitle) == "" {
		return summaryResult{}, errors.New("Summary generation request requires a chat title")
	}
	if !dayKeyFormat.MatchString(request.DayKeyUTC) {
		return summaryResult{}, errors.New("Summary generation request requires dayKeyUtc in YYYY-MM-DD format")
	}
	if strings.TrimSpace(request.Transcript) == "" {
		return summaryResult{}, errors.New("Summary generation request requires a non-empty transcript")
	}
	participants := dedupeParticipants(request.Participants)
	var topics []summaryTopic
	topicAttempts, err := g.run(ctx, topicProgram, user([][2]string{
		{"Chat Title", request.ChatTitle}, {"Day Key Utc", request.DayKeyUTC}, {"Participants", jsonIndent(participants)},
		{"Message Transcript", request.Transcript}, {"Quality Rules", qualityRules},
	}), func(v any) (err error) {
		topics, err = validateTopics(v, participants)
		return err
	})
	if err != nil {
		return summaryResult{}, err
	}
	var oneliner string
	onelinerAttempts, err := g.run(ctx, onelinerProgram, user([][2]string{
		{"Chat Title", request.ChatTitle}, {"Day Key Utc", request.DayKeyUTC}, {"Topics", jsonIndent(topics)},
	}), func(v any) (err error) {
		oneliner, err = validateOneliner(v)
		oneliner = jsSlice(oneliner, maxOnelinerChars)
		return err
	})
	if err != nil {
		return summaryResult{}, err
	}
	return summaryResult{Topics: topics, Oneliner: oneliner, Attempts: topicAttempts + onelinerAttempts}, nil
}

type retryExhausted struct {
	message string
	reasons []string
}

func (e *retryExhausted) Error() string { return e.message }

func (g *summaryGenerator) run(ctx context.Context, program summaryProgram, prompt string, accept func(any) error) (int, error) {
	system := program.system()
	delay := 150 * time.Millisecond
	var reasons []string
	var last error
	for attempt := 1; attempt <= g.maxAttempts; attempt++ {
		content, err := g.llm.complete(ctx, g.model, system, prompt)
		if err == nil {
			var value any
			if value, err = program.parse(content); err == nil {
				err = accept(value)
			}
		}
		if err == nil {
			return attempt, nil
		}
		if ctx.Err() != nil {
			return attempt, ctx.Err()
		}
		last = err
		reasons = append(reasons, err.Error())
		if attempt == g.maxAttempts {
			break
		}
		jitter := float64(delay) * 0.15 * (rand.Float64()*2 - 1)
		select {
		case <-time.After(delay + time.Duration(jitter)):
		case <-ctx.Done():
			return attempt, ctx.Err()
		}
		delay = min(1200*time.Millisecond, delay*2)
	}
	return g.maxAttempts, &retryExhausted{message: fmt.Sprintf("%s failed after %d attempts: %v", program.label, g.maxAttempts, last), reasons: reasons}
}
