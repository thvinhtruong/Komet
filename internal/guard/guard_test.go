package guard

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	in := `export OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwxyz123456
db: postgres://admin:hunter2pass@db.internal:5432/app
password = "s3cretValue"
aws AKIAABCDEFGHIJKLMNOP
nothing secret here`
	out, fs := Redact(in)
	for _, leak := range []string{"sk-abcdefghij", "hunter2pass", "s3cretValue", "AKIAABCDEFGHIJKLMNOP"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "postgres://admin:[REDACTED:connection_string]@db.internal") {
		t.Errorf("connection string should keep host and user:\n%s", out)
	}
	if !strings.Contains(out, "nothing secret here") {
		t.Error("clean text was altered")
	}
	if len(fs) != 4 {
		t.Errorf("want 4 findings, got %d: %+v", len(fs), fs)
	}
}

func TestNoFalsePositive(t *testing.T) {
	for _, s := range []string{
		"find files larger than 100MB",
		"how do I reset my password on ubuntu",
		"git log --oneline | head",
	} {
		if fs := Scan(s); len(fs) != 0 {
			t.Errorf("%q: unexpected findings %+v", s, fs)
		}
	}
}
