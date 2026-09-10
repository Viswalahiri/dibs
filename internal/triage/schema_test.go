package triage

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The JSON schema sent to the API and the Go struct decoded from the response
// have to describe the same object. Nothing enforces that at compile time, and
// a mismatch is silent: the API constrains the model to a field the decoder
// then rejects as unknown. This walks both and compares them.
func TestSchemaMatchesTheGoTypes(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(responseSchema, &schema); err != nil {
		t.Fatalf("schema.json is not valid JSON: %v", err)
	}

	compare(t, "Response", schema.Properties, reflect.TypeOf(Response{}))

	// The nested objects, keyed by the field that holds them.
	nested := map[string]reflect.Type{
		"vetoes":     reflect.TypeOf(Vetoes{}),
		"dimensions": reflect.TypeOf(Dimensions{}),
		"effort":     reflect.TypeOf(Effort{}),
	}
	for field, typ := range nested {
		var obj struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(schema.Properties[field], &obj); err != nil {
			t.Fatalf("%s: %v", field, err)
		}
		compare(t, field, obj.Properties, typ)
	}

	defs := map[string]reflect.Type{
		"veto":      reflect.TypeOf(Veto{}),
		"dimension": reflect.TypeOf(Dimension{}),
	}
	for name, typ := range defs {
		var obj struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(schema.Defs[name], &obj); err != nil {
			t.Fatalf("$defs.%s: %v", name, err)
		}
		compare(t, "$defs."+name, obj.Properties, typ)
	}
}

func compare(t *testing.T, where string, properties map[string]json.RawMessage, typ reflect.Type) {
	t.Helper()
	tags := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := cutTag(typ.Field(i).Tag.Get("json"))
		if name == "" || name == "-" {
			continue
		}
		tags[name] = true
		if _, ok := properties[name]; !ok {
			t.Errorf("%s: struct field %q has no property in schema.json", where, name)
		}
	}
	for name := range properties {
		if !tags[name] {
			t.Errorf("%s: schema.json property %q has no struct field", where, name)
		}
	}
}

func cutTag(tag string) (name, opts string, ok bool) {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i], tag[i+1:], true
		}
	}
	return tag, "", false
}

// Decoding refuses unknown fields, so a schema change that adds a property
// without a matching struct field fails loudly instead of being dropped.
func TestDecodeRejectsUnknownFields(t *testing.T) {
	_, err := Decode([]byte(`{"vetoes":{},"dimensions":{},"effort":{},"stack":[],
		"positives":[],"top_risk":"","surprise":1}`))
	if err == nil {
		t.Fatal("an unknown field decoded without complaint")
	}
}

func TestValidate(t *testing.T) {
	good := func() Response {
		d := Dimension{Score: 4, Why: "cites the reproduction steps in the body"}
		return Response{
			Dimensions: Dimensions{
				ScopeClarity: d, Concreteness: d, BlastRadius: d,
				MaintainerInvitation: d, ContentionRisk: d,
			},
			Positives: []string{"repro included"},
		}
	}

	if err := good().Validate(); err != nil {
		t.Fatalf("a good response was rejected: %v", err)
	}

	t.Run("a five with no reasoning is rejected", func(t *testing.T) {
		r := good()
		r.Dimensions.BlastRadius = Dimension{Score: 5, Why: "small"}
		if err := r.Validate(); err == nil {
			t.Fatal("an unjustified 5 passed validation")
		}
		r.DowngradeUnjustifiedFives()
		if r.Dimensions.BlastRadius.Score != 4 {
			t.Fatalf("downgrade left the score at %d, want 4", r.Dimensions.BlastRadius.Score)
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("the downgraded response still fails: %v", err)
		}
	})

	t.Run("a justified five is kept", func(t *testing.T) {
		r := good()
		r.Dimensions.BlastRadius = Dimension{Score: 5, Why: "one function in pool.go, no callers outside it"}
		if err := r.Validate(); err != nil {
			t.Fatalf("a justified 5 was rejected: %v", err)
		}
		r.DowngradeUnjustifiedFives()
		if r.Dimensions.BlastRadius.Score != 5 {
			t.Error("a justified 5 was downgraded")
		}
	})

	t.Run("a confidence outside zero to one is rejected", func(t *testing.T) {
		r := good()
		r.Vetoes.SelfFixing.Confidence = 1.4
		if err := r.Validate(); err == nil {
			t.Fatal("an out-of-range confidence passed validation")
		}
	})

	t.Run("no positives is rejected", func(t *testing.T) {
		r := good()
		r.Positives = nil
		if err := r.Validate(); err == nil {
			t.Fatal("a response with no positives passed validation")
		}
	})
}
