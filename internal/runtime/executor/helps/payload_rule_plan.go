package helps

import (
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// payloadRuleDocument lets rule evaluation use either the business document or
// a private projection when built-in cleanup depends on the evaluated effects.
type payloadRuleDocument interface {
	Get(string) ojson.Result
	Set(string, any) error
	SetRaw(string, string) error
	SetRawBytes(string, []byte) error
	Delete(string) error
}

type payloadRuleOperation struct {
	path           string
	value          string
	remove         bool
	autoImageIndex int
}

// payloadRulePlan retains resolved operations, including their encoded values.
// Reads see preceding effects without changing the business document. Ordinary
// paths copy only their first changed field into the private Doc; cross-root
// queries fall back to a complete projection using the same retained operations.
type payloadRulePlan struct {
	source         *ojson.Doc
	effects        *ojson.Doc
	copied         map[string]bool
	complete       bool
	operations     []payloadRuleOperation
	autoImageIndex *int
}

func newPayloadRulePlan(source *ojson.Doc, autoImageIndex *int) *payloadRulePlan {
	return &payloadRulePlan{source: source, effects: ojson.NewDocString(`{}`), copied: make(map[string]bool), autoImageIndex: autoImageIndex}
}

func (p *payloadRulePlan) field(path string) string {
	parts := splitPayloadRulePath(path)
	if len(parts) > 0 && parts[0] != "" && !strings.ContainsAny(parts[0], `\#@|[{}:`) && !strings.Contains(path, "|") {
		return parts[0]
	}
	if !p.complete {
		p.effects = ojson.NewDocString(p.source.Get("@this").Raw)
		for _, op := range p.operations {
			_ = op.apply(p.effects, op.path)
		}
		p.complete = true
		p.copied = nil
	}
	return ""
}

func (p *payloadRulePlan) Get(path string) ojson.Result {
	field := p.field(path)
	if p.complete || p.copied[field] {
		return p.effects.Get(path)
	}
	return p.source.Get(path)
}

func (p *payloadRulePlan) writable(path string) *ojson.Doc {
	field := p.field(path)
	if !p.complete && !p.copied[field] {
		if value := p.source.Get(field); value.Exists() {
			_ = p.effects.SetRaw(field, value.Raw)
		}
		p.copied[field] = true
	}
	return p.effects
}

func (p *payloadRulePlan) Set(path string, value any) error {
	encoded := ojson.NewDocString(`{}`)
	if err := encoded.Set("value", value); err != nil {
		return err
	}
	return p.SetRaw(path, encoded.Get("value").Raw)
}

func (p *payloadRulePlan) SetRaw(path, value string) error {
	if err := p.writable(path).SetRaw(path, value); err != nil {
		return err
	}
	p.operations = append(p.operations, payloadRuleOperation{path: path, value: strings.Clone(value), autoImageIndex: *p.autoImageIndex})
	return nil
}

func (p *payloadRulePlan) SetRawBytes(path string, value []byte) error {
	return p.SetRaw(path, string(value))
}

func (p *payloadRulePlan) Delete(path string) error {
	if err := p.writable(path).Delete(path); err != nil {
		return err
	}
	p.operations = append(p.operations, payloadRuleOperation{path: path, remove: true, autoImageIndex: *p.autoImageIndex})
	return nil
}

func (op payloadRuleOperation) apply(doc *ojson.Doc, path string) error {
	if op.remove {
		return doc.Delete(path)
	}
	return doc.SetRaw(path, op.value)
}

// apply commits the evaluated operations once, after automatic-image cleanup.
// Numeric tool indexes still identify the elements used during evaluation;
// removing the automatic element must not shift an explicit write to a sibling.
func (p *payloadRulePlan) apply(doc *ojson.Doc, toolsPath string, removedAutoImage bool) bool {
	changed := false
	for _, op := range p.operations {
		path := op.path
		if removedAutoImage && op.autoImageIndex >= 0 && strings.HasPrefix(path, toolsPath+".") {
			index, tail, _ := strings.Cut(strings.TrimPrefix(path, toolsPath+"."), ".")
			if number, err := strconv.Atoi(index); err == nil && number >= 0 {
				if number == op.autoImageIndex && op.remove {
					continue
				}
				if number > op.autoImageIndex {
					path = toolsPath + "." + strconv.Itoa(number-1)
					if tail != "" {
						path += "." + tail
					}
				}
			}
		}
		if err := op.apply(doc, path); err == nil {
			changed = true
		}
	}
	return changed
}
