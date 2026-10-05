package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// —— question generation ——

func TestGenMathQuestionShapes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 300; i++ {
		q := genMathQuestion()
		if q.question == "" || q.answer == "" {
			t.Fatalf("empty question or answer: %+v", q)
		}
		// The answer must be the question evaluated.
		want, ok := evalQuestion(q.question)
		if !ok {
			t.Fatalf("unparsable question: %q", q.question)
		}
		if want != q.answer {
			t.Fatalf("question %q answer %q, computed %q", q.question, q.answer, want)
		}
		// The question is one of the source's shapes.
		shape := q.question
		for _, r := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", " "} {
			shape = strings.ReplaceAll(shape, r, "#")
		}
		seen[shape] = true
	}
	if len(seen) < 4 {
		t.Fatalf("too few distinct shapes: %v", seen)
	}
}

// evalQuestion computes a generated question's answer.
func evalQuestion(q string) (string, bool) {
	var a, b, c int
	var op string
	q = strings.TrimSpace(q)
	if strings.HasSuffix(q, "²") {
		if _, err := fmt.Sscanf(q, "%d²", &a); err != nil {
			return "", false
		}
		return fmt.Sprint(a * a), true
	}
	if n, _ := fmt.Sscanf(q, "%d × %d + %d", &a, &b, &c); n == 3 {
		return fmt.Sprint(a*b + c), true
	}
	if n, _ := fmt.Sscanf(q, "%d %s %d", &a, &op, &b); n == 3 {
		switch op {
		case "+":
			return fmt.Sprint(a + b), true
		case "-":
			return fmt.Sprint(a - b), true
		case "×":
			return fmt.Sprint(a * b), true
		case "÷":
			if b == 0 {
				return "", false
			}
			return fmt.Sprint(a / b), true
		}
	}
	return "", false
}

func TestRandIntRange(t *testing.T) {
	for i := 0; i < 200; i++ {
		n := randInt(3, 9)
		if n < 3 || n > 9 {
			t.Fatalf("randInt out of range: %d", n)
		}
	}
	if randInt(5, 5) != 5 {
		t.Fatal("degenerate range")
	}
}

// —— answer grading ——

func TestAnswerCorrect(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"42", "42", true},
		{" 42 ", "42", true},
		{"abc", "ABC", true},
		{"", "42", false},
		{"43", "42", false},
		{"4 2", "42", false},
	}
	for _, c := range cases {
		if got := answerCorrect(c.in, c.want); got != c.ok {
			t.Fatalf("answerCorrect(%q,%q) = %v want %v", c.in, c.want, got, c.ok)
		}
	}
}

// —— challenge expiry ——

func TestChallengeExpiry(t *testing.T) {
	now := time.Now()
	c := &challenge{answer: "42", started: now}
	if c.expired(0, now) {
		t.Fatal("no timeout must never expire")
	}
	if c.expired(30, now) {
		t.Fatal("fresh challenge expired")
	}
	if !c.expired(30, now.Add(31*time.Second)) {
		t.Fatal("challenge past timeout not expired")
	}
	if !c.expired(30, now.Add(30*time.Second+time.Millisecond)) {
		t.Fatal("boundary expiry failed")
	}
}

// —— persistence ——

func TestRecordsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), dataFile)
	in := newRecords()
	in.addWhitelist(1)
	in.addVerified(2, "Alice", "")
	in.addFailed(3, "Bob", reasonTimeout)
	if err := writeJSON(path, in); err != nil {
		t.Fatal(err)
	}
	var out records
	if err := readJSON(path, &out); err != nil {
		t.Fatal(err)
	}
	out.ensure()
	if !out.inWhitelist(1) || out.inWhitelist(2) {
		t.Fatal("whitelist round trip failed")
	}
	if !out.isVerified(2) {
		t.Fatal("verified round trip failed")
	}
	if _, ok := out.getFailed(3); !ok {
		t.Fatal("failed round trip failed")
	}
	if out.nameOf(2) != "Alice" || out.nameOf(3) != "Bob" {
		t.Fatal("name lookup failed")
	}
}

func TestReadJSONMissing(t *testing.T) {
	var r records
	if err := readJSON(filepath.Join(t.TempDir(), "nope.json"), &r); err != nil {
		t.Fatal(err)
	}
}

func TestRecordsMutations(t *testing.T) {
	r := newRecords()
	r.addWhitelist(1)
	r.addVerified(2, "Alice", "")
	r.addFailed(3, "Bob", reasonMaxTries)
	// addWhitelist removes the verified record, like the source.
	r.addWhitelist(2)
	if r.isVerified(2) {
		t.Fatal("whitelisting must clear the verified record")
	}
	r.delWhitelist(2)
	if r.inWhitelist(2) {
		t.Fatal("delWhitelist failed")
	}
	// addFailed is idempotent per user.
	r.addFailed(3, "Bob", reasonTimeout)
	if len(r.Failed) != 1 {
		t.Fatalf("duplicate failed record: %d", len(r.Failed))
	}
	// nameOf falls back across records.
	if r.nameOf(1) != "" {
		t.Fatal("whitelist stores no name")
	}
	if r.nameOf(3) != "Bob" {
		t.Fatal("failed name lookup")
	}
	// clearing.
	r.clearWhitelist()
	r.clearVerified()
	r.clearFailed()
	if len(r.Whitelist)+len(r.Verified)+len(r.Failed) != 0 {
		t.Fatal("clear failed")
	}
}

// —— helpers ——

func TestDigitsOnlyAndParse(t *testing.T) {
	if !digitsOnly("123") || digitsOnly("12a") || digitsOnly("") || digitsOnly("-1") {
		t.Fatal("digitsOnly")
	}
	if _, err := parseInt64(" 42 "); err != nil {
		t.Fatal(err)
	}
	if _, err := parseInt64("x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestClipLines(t *testing.T) {
	s := "> `aaa`\n> `bbb`\n> `ccc`"
	if got := clipLines(s, 12); got != "> `aaa`\n…" {
		t.Fatalf("got %q", got)
	}
	if clipLines(s, 100) != s {
		t.Fatal("short text changed")
	}
}

func TestSortIDsAndTime(t *testing.T) {
	ids := []int64{3, 1, 2}
	sortIDs(ids)
	if !reflect.DeepEqual(ids, []int64{1, 2, 3}) {
		t.Fatalf("sorted %v", ids)
	}
	r := newRecords()
	base := time.Now()
	r.Verified = map[string]verifiedRecord{
		"1": {ID: 1, Time: base},
		"2": {ID: 2, Time: base.Add(time.Hour)},
	}
	got := r.verifiedIDs()
	if len(got) != 2 || got[0] != 2 {
		t.Fatalf("newest-first failed: %v", got)
	}
}

func TestFailActionLabel(t *testing.T) {
	if failActionLabel(failBlock) == failActionLabel(failReport) {
		t.Fatal("labels collide")
	}
	if failActionLabel("none") != "仅归档并静音" {
		t.Fatal("default label")
	}
}

func TestFirstArg(t *testing.T) {
	if firstArg(nil) != "" || firstArg([]string{"ABC"}) != "abc" {
		t.Fatal("firstArg")
	}
}
