package limits

import "testing"

func ptr(v int) *int { return &v }
func TestResolvePerDimensionPrecedence(t *testing.T) {
	l := Limits{Default: ModelLimits{TPM: ptr(100), RPM: ptr(10)}, PerModel: map[string]ModelLimits{"a/b:free": {TPM: ptr(50)}}, Rules: []Rule{{Match: "*:free", Bucket: "free-bucket", Free: true, ModelLimits: ModelLimits{RPM: ptr(2), RPD: ptr(8)}}}}
	r := l.Resolve("a/b:free")
	if *r.TPM != 50 || *r.RPM != 2 || *r.RPD != 8 || r.Bucket != "free-bucket" || !r.Free {
		t.Fatalf("unexpected: %+v", r)
	}
}
func TestGlobAcrossSlash(t *testing.T) {
	if !GlobMatch("*:free", "a/b:free") {
		t.Fatal("* must cross slash")
	}
	if !GlobMatch("a/?/c", "a/b/c") {
		t.Fatal("? must match one rune")
	}
	if GlobMatch("a/?", "a/bb") {
		t.Fatal("unexpected match")
	}
}
