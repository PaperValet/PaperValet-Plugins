package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errNoTarget = errors.New("NO_TARGET")

// ============================================================
// Storage: data/pmcaptcha/data.json
// ============================================================

const (
	reasonTimeout  = "timeout"
	reasonMaxTries = "max_tries"
)

type verifiedRecord struct {
	ID   int64     `json:"id"`
	Name string    `json:"name,omitempty"`
	Time time.Time `json:"time"`
}

type failedRecord struct {
	ID     int64     `json:"id"`
	Name   string    `json:"name,omitempty"`
	Reason string    `json:"reason"`
	Time   time.Time `json:"time"`
}

type records struct {
	mu        sync.Mutex                `json:"-"`
	Whitelist map[string]bool           `json:"whitelist"` // user id -> true
	Verified  map[string]verifiedRecord `json:"verified"`
	Failed    map[string]failedRecord   `json:"failed"`
}

func newRecords() *records {
	return &records{Whitelist: map[string]bool{}, Verified: map[string]verifiedRecord{}, Failed: map[string]failedRecord{}}
}

// save marshals and writes the store under its lock.
func (r *records) save(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return writeJSON(path, r)
}

func (r *records) ensure() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Whitelist == nil {
		r.Whitelist = map[string]bool{}
	}
	if r.Verified == nil {
		r.Verified = map[string]verifiedRecord{}
	}
	if r.Failed == nil {
		r.Failed = map[string]failedRecord{}
	}
}

func keyOf(id int64) string { return strconv.FormatInt(id, 10) }

func (r *records) inWhitelist(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Whitelist[keyOf(id)]
}

func (r *records) addWhitelist(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Whitelist[keyOf(id)] = true
	delete(r.Verified, keyOf(id))
}

func (r *records) delWhitelist(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.Whitelist, keyOf(id))
}

func (r *records) clearWhitelist() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Whitelist = map[string]bool{}
}

func (r *records) isVerified(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.Verified[keyOf(id)]
	return ok
}

func (r *records) addVerified(id int64, name, username string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" {
		name = strconv.FormatInt(id, 10)
	}
	r.Verified[keyOf(id)] = verifiedRecord{ID: id, Name: name, Time: time.Now()}
}

func (r *records) delVerified(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.Verified, keyOf(id))
}

func (r *records) clearVerified() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Verified = map[string]verifiedRecord{}
}

func (r *records) getVerified(id int64) (verifiedRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.Verified[keyOf(id)]
	return v, ok
}

func (r *records) addFailed(id int64, name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" {
		name = strconv.FormatInt(id, 10)
	}
	r.Failed[keyOf(id)] = failedRecord{ID: id, Name: name, Reason: reason, Time: time.Now()}
}

func (r *records) delFailed(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.Failed, keyOf(id))
}

func (r *records) clearFailed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Failed = map[string]failedRecord{}
}

func (r *records) getFailed(id int64) (failedRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.Failed[keyOf(id)]
	return f, ok
}

// nameOf returns the best stored name for a user id ("" when unknown).
func (r *records) nameOf(id int64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.Verified[keyOf(id)]; ok && v.Name != "" {
		return v.Name
	}
	if f, ok := r.Failed[keyOf(id)]; ok && f.Name != "" {
		return f.Name
	}
	return ""
}

func (r *records) counts() (wl, verified, failed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Whitelist), len(r.Verified), len(r.Failed)
}

// whitelistIDs returns the whitelist as sorted ids.
func (r *records) whitelistIDs() []int64 {
	r.mu.Lock()
	ids := make([]int64, 0, len(r.Whitelist))
	for k := range r.Whitelist {
		if id, err := parseInt64(k); err == nil {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	sortIDs(ids)
	return ids
}

// verifiedIDs returns verified ids sorted by verification time (newest first).
func (r *records) verifiedIDs() []int64 {
	r.mu.Lock()
	m := make(map[string]verifiedRecord, len(r.Verified))
	for k, v := range r.Verified {
		m[k] = v
	}
	r.mu.Unlock()
	ids := make([]int64, 0, len(m))
	for _, v := range m {
		ids = append(ids, v.ID)
	}
	sortIDsDescTime(m, ids)
	return ids
}

// failedIDs returns failed ids sorted by failure time (newest first).
func (r *records) failedIDs() []int64 {
	r.mu.Lock()
	m := make(map[string]failedRecord, len(r.Failed))
	for k, v := range r.Failed {
		m[k] = v
	}
	r.mu.Unlock()
	ids := make([]int64, 0, len(m))
	for _, f := range m {
		ids = append(ids, f.ID)
	}
	sortIDsDescTimeF(m, ids)
	return ids
}

func sortIDs(ids []int64) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

func sortIDsDescTime(m map[string]verifiedRecord, ids []int64) {
	for i := 1; i < len(ids); i++ {
		ti := m[keyOf(ids[i])].Time
		for j := i; j > 0 && m[keyOf(ids[j-1])].Time.Before(ti); j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

func sortIDsDescTimeF(m map[string]failedRecord, ids []int64) {
	for i := 1; i < len(ids); i++ {
		ti := m[keyOf(ids[i])].Time
		for j := i; j > 0 && m[keyOf(ids[j-1])].Time.Before(ti); j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

// ============================================================
// Challenge state
// ============================================================

type challenge struct {
	answer  string
	tries   int
	msgIDs  []int
	started time.Time
}

// expired reports whether the challenge outlived its timeout (0 = never).
func (c *challenge) expired(timeout int, now time.Time) bool {
	if timeout <= 0 {
		return false
	}
	return now.Sub(c.started) > time.Duration(timeout)*time.Second
}

// answerCorrect grades input against the answer, case-insensitive, ignoring
// surrounding whitespace.
func answerCorrect(input, answer string) bool {
	in := strings.ToUpper(strings.TrimSpace(input))
	want := strings.ToUpper(strings.TrimSpace(answer))
	return in != "" && in == want
}

// ============================================================
// Math question generation (crypto/rand, like the source's variety)
// ============================================================

type mathQuestion struct{ question, answer string }

// randInt returns a uniform int in [min, max] using crypto/rand.
func randInt(min, max int) int {
	if max <= min {
		return min
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max-min+1)))
	if err != nil {
		return min
	}
	return min + int(n.Int64())
}

// genMathQuestion builds one random question, mirroring the source's six
// shapes: a+b, a-b, a×b, a÷b, a×b+c, a².
func genMathQuestion() mathQuestion {
	switch randInt(0, 5) {
	case 0:
		a, b := randInt(10, 99), randInt(10, 99)
		return mathQuestion{question: fmt.Sprintf("%d + %d", a, b), answer: strconv.Itoa(a + b)}
	case 1:
		b := randInt(10, 60)
		a := randInt(b+1, b+60)
		return mathQuestion{question: fmt.Sprintf("%d - %d", a, b), answer: strconv.Itoa(a - b)}
	case 2:
		a, b := randInt(2, 9), randInt(11, 25)
		return mathQuestion{question: fmt.Sprintf("%d × %d", a, b), answer: strconv.Itoa(a * b)}
	case 3:
		divisor, quotient := randInt(2, 12), randInt(3, 15)
		return mathQuestion{question: fmt.Sprintf("%d ÷ %d", divisor*quotient, divisor), answer: strconv.Itoa(quotient)}
	case 4:
		a, b, c := randInt(2, 9), randInt(2, 9), randInt(1, 20)
		return mathQuestion{question: fmt.Sprintf("%d × %d + %d", a, b, c), answer: strconv.Itoa(a*b + c)}
	default:
		a := randInt(2, 12)
		return mathQuestion{question: fmt.Sprintf("%d²", a), answer: strconv.Itoa(a * a)}
	}
}

// ============================================================
// Small helpers
// ============================================================

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseInt64(s string) (int64, error) { return strconv.ParseInt(strings.TrimSpace(s), 10, 64) }

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return strings.ToLower(args[0])
}

func fmtTime(t time.Time) string { return t.Format("2006-01-02 15:04") }
