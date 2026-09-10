package triage

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// responseSchema is passed as output_config.format so the API constrains the
// shape of what comes back. Every object in it sets additionalProperties:
// false, and dimension scores are an enum rather than a numeric range because
// the API's structured outputs reject minimum and maximum.
//
// Everything the schema cannot express (word caps, exactly two positives) is
// asked for in the prompt and checked by Validate.
//
//go:embed schema.json
var responseSchema []byte

// Response is the model's verdict on one issue. It never contains a final
// score: Go computes the composite, because a model-produced number drifts
// between calls and makes the weights impossible to retune.
type Response struct {
	Vetoes     Vetoes     `json:"vetoes"`
	Dimensions Dimensions `json:"dimensions"`
	Effort     Effort     `json:"effort"`
	Stack      []string   `json:"stack"`
	Positives  []string   `json:"positives"`
	TopRisk    string     `json:"top_risk"`
}

// Veto is one of the three dead ends, with how sure the model is and the text
// that convinced it. Confidence below the configured threshold becomes a score
// penalty rather than a kill.
type Veto struct {
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
}

type Vetoes struct {
	SelfFixing   Veto `json:"self_fixing"`
	AlreadyTaken Veto `json:"already_taken"`
	PoorlyScoped Veto `json:"poorly_scoped"`
}

// Each returns the three vetoes with the reject reason each one carries, in a
// fixed order so a score is reproducible.
func (v Vetoes) Each() []NamedVeto {
	return []NamedVeto{
		{"self_fixing", v.SelfFixing},
		{"already_taken", v.AlreadyTaken},
		{"poorly_scoped", v.PoorlyScoped},
	}
}

type NamedVeto struct {
	Name string
	Veto Veto
}

type Dimension struct {
	Score int    `json:"score"`
	Why   string `json:"why"`
}

type Dimensions struct {
	ScopeClarity         Dimension `json:"scope_clarity"`
	Concreteness         Dimension `json:"concreteness"`
	BlastRadius          Dimension `json:"blast_radius"`
	MaintainerInvitation Dimension `json:"maintainer_invitation"`
	ContentionRisk       Dimension `json:"contention_risk"`
}

func (d Dimensions) Each() []NamedDimension {
	return []NamedDimension{
		{"scope_clarity", d.ScopeClarity},
		{"concreteness", d.Concreteness},
		{"blast_radius", d.BlastRadius},
		{"maintainer_invitation", d.MaintainerInvitation},
		{"contention_risk", d.ContentionRisk},
	}
}

type NamedDimension struct {
	Name      string
	Dimension Dimension
}

type Effort struct {
	LowHours   float64 `json:"low_hours"`
	HighHours  float64 `json:"high_hours"`
	Confidence string  `json:"confidence"`
}

// minWhyChars is the guard against a compliant model. A 5 justified in three
// words is not a judgment, it is agreement.
const minWhyChars = 20

// Validate reports what the schema could not enforce. A failure here triggers
// one retry with a sharper instruction; the caller decides what to do after
// that.
func (r Response) Validate() error {
	var problems []string
	for _, d := range r.Dimensions.Each() {
		if d.Dimension.Score < 1 || d.Dimension.Score > 5 {
			problems = append(problems,
				fmt.Sprintf("%s scored %d, want 1 to 5", d.Name, d.Dimension.Score))
		}
		if d.Dimension.Score == 5 && len(strings.TrimSpace(d.Dimension.Why)) < minWhyChars {
			problems = append(problems,
				fmt.Sprintf("%s scored 5 with no reasoning: %q", d.Name, d.Dimension.Why))
		}
	}
	for _, v := range r.Vetoes.Each() {
		if v.Veto.Confidence < 0 || v.Veto.Confidence > 1 {
			problems = append(problems,
				fmt.Sprintf("%s confidence %v is outside 0 to 1", v.Name, v.Veto.Confidence))
		}
	}
	if len(r.Positives) == 0 {
		problems = append(problems, "no positives")
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid triage response: %s", strings.Join(problems, "; "))
	}
	return nil
}

// DowngradeUnjustifiedFives drops a 5 that came with no reasoning to a 4. It
// runs after the retry has also failed, so a model that will not justify its
// top marks cannot keep them.
func (r *Response) DowngradeUnjustifiedFives() {
	for _, d := range []*Dimension{
		&r.Dimensions.ScopeClarity, &r.Dimensions.Concreteness,
		&r.Dimensions.BlastRadius, &r.Dimensions.MaintainerInvitation,
		&r.Dimensions.ContentionRisk,
	} {
		if d.Score == 5 && len(strings.TrimSpace(d.Why)) < minWhyChars {
			d.Score = 4
		}
	}
}

func Decode(raw []byte) (Response, error) {
	var r Response
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Response{}, fmt.Errorf("decode triage response: %w", err)
	}
	return r, nil
}
