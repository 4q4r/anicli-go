package rules

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const transformFixture = `{"data": [{"link": "//cdn.example/v/1"}]}`

func parseTransformFixture(t *testing.T) any {
	t.Helper()
	var data any
	if err := json.Unmarshal([]byte(transformFixture), &data); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return data
}

func transformRules(tr Transform) []Rule {
	return []Rule{
		{Path: "data", Multiple: true},
		{Path: "link", Attr: AttrURL, Transform: tr},
	}
}

func TestUnknownTransformRejectedLoudly(t *testing.T) {
	t.Parallel()

	data := parseTransformFixture(t)

	_, err := Extract(data, transformRules("urldecode"))
	if err == nil {
		t.Fatal("extract with unknown transform: want error, got nil (silent typo passthrough)")
	}
	if !strings.Contains(err.Error(), "transform") {
		t.Errorf("error = %v, want it to name the transform", err)
	}
	var re *RuleError
	if !errors.As(err, &re) {
		t.Errorf("error = %v, want *rules.RuleError", err)
	}

	// Construction-time rejection must agree with extraction-time.
	if err := transformRules("urldecode")[1].Validate(); err == nil {
		t.Error("Rule.Validate on unknown transform: want error, got nil")
	}
}

func TestValidateAcceptsKnownTransforms(t *testing.T) {
	t.Parallel()

	for _, tr := range []Transform{TransformNone, TransformProtocolRelative} {
		r := Rule{Path: "link", Attr: AttrURL, Transform: tr}
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", tr, err)
		}
	}
}

func TestProtocolRelativeStillTransforms(t *testing.T) {
	t.Parallel()

	data := parseTransformFixture(t)

	got, err := Extract(data, transformRules(TransformProtocolRelative))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(got) != 1 || got[0].URL != "https://cdn.example/v/1" {
		t.Errorf("results = %+v, want protocol-relative URL rewritten", got)
	}
}
