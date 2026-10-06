package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeCategory(t *testing.T) {
	for in, want := range map[string]string{"": "", "dongman": "dongman", "MEINV": "meizi", "风景": "fengjing", "random": "suiji"} {
		got, ok := normalizeCategory(in)
		if !ok || got != want {
			t.Errorf("%q → %q %v", in, got, ok)
		}
	}
	if _, ok := normalizeCategory("xyz"); ok {
		t.Fatal("xyz accepted")
	}
}

func TestQuery(t *testing.T) {
	for i := 0; i < 50; i++ {
		q := buildWallhavenQuery(categories["dongman"])
		if q.Get("purity") != "100" || q.Get("ratios") != "16x9" || q.Get("categories") != "010" || q.Get("q") == "" {
			t.Fatal(q.Encode())
		}
		if (q.Get("sorting") == "random") != (q.Get("seed") != "") {
			t.Fatal(q.Encode())
		}
		if n := len(strings.Split(q.Get("q"), "+")); n < 1 || n > 2 {
			t.Fatal(q.Get("q"))
		}
	}
	if q := buildWallhavenQuery(categories["suiji"]); q.Has("categories") {
		t.Fatal("suiji should not filter categories")
	}
}

func TestQualified(t *testing.T) {
	l := []whWallpaper{{ID: "a", FileSize: 4 << 20, DimX: 1920, DimY: 1080}, {ID: "b", FileSize: 1 << 20, DimX: 3840, DimY: 2160}}
	if q := qualified(l, 1920, 1080); len(q) != 1 || q[0].ID != "a" {
		t.Fatal(q)
	}
	if q := qualified(l, 2560, 1440); len(q) != 0 {
		t.Fatal(q)
	}
}

func TestCleanup(t *testing.T) {
	wd, _ := os.Getwd()
	tmp := t.TempDir()
	os.Chdir(tmp)
	defer os.Chdir(wd)
	os.MkdirAll(dataDir, 0o755)
	dir, _ := os.MkdirTemp(dataDir, "dl-")
	f := filepath.Join(dir, "x.jpg")
	os.WriteFile(f, []byte("x"), 0o644)
	cleanup(f)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("dir not removed")
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatal("dataDir removed")
	}
}

func TestCategoryChoices(t *testing.T) {
	cs := categoryChoices()
	if len(cs) != 5 || cs[0].Value != "" {
		t.Fatalf("%+v", cs)
	}
	for _, c := range cs {
		if c.Value == "" {
			continue
		}
		if _, ok := categories[c.Value]; !ok {
			t.Fatalf("choice %s not a category", c.Value)
		}
	}
}

func TestBtstuExt(t *testing.T) {
	cases := map[string]string{
		"https://example.com/a/b/img.png":     ".png",
		"https://example.com/img.JPG":         ".jpg",
		"https://example.com/x.webp?q=1":      ".webp",
		"https://example.com/noext":           ".jpg",
		"https://example.com/":                ".jpg",
		"https://example.com/.weirdext7/file": ".jpg", // too long → default
	}
	for in, want := range cases {
		if got := btstuExt(in); got != want {
			t.Errorf("btstuExt(%q) = %q, want %q", in, got, want)
		}
	}
}
