package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func testManifest() map[string]any {
	return map[string]any{
		"id":      "xqs-vnc",
		"version": "0.1.0",
		"engine": map[string]any{
			"entry": "xqs-vnc",
		},
	}
}

func writeFakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("fake binary contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBuildAndVerifyBundle_RoundTrip proves a correctly built and signed
// bundle verifies successfully end-to-end, without requiring a real
// cross-compiled plugin binary.
func TestBuildAndVerifyBundle_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	binPath := writeFakeBinary(t, dir, "xqs-vnc")

	uiFiles := []bundleFile{
		{arcName: "ui/index.html", data: []byte("<html></html>")},
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	bundlePath := filepath.Join(dir, "xqs-vnc-linux-amd64.xqsp")
	sums, sig, err := buildBundle(bundlePath, testManifest(), binPath, "xqs-vnc", uiFiles, priv)
	if err != nil {
		t.Fatalf("buildBundle: %v", err)
	}
	if len(sums) != 3 { // plugin.json, xqs-vnc, ui/index.html
		t.Fatalf("expected 3 checksum entries, got %d: %v", len(sums), sums)
	}
	if sig == "" {
		t.Fatal("expected non-empty signature")
	}

	if err := verifyBundle(bundlePath, pub); err != nil {
		t.Fatalf("verifyBundle failed on a correctly signed bundle: %v", err)
	}
}

// TestVerifyBundle_TamperedFile proves that modifying a file's contents
// inside the archive after signing is detected as a checksum mismatch.
func TestVerifyBundle_TamperedFile(t *testing.T) {
	dir := t.TempDir()
	binPath := writeFakeBinary(t, dir, "xqs-vnc")

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	bundlePath := filepath.Join(dir, "xqs-vnc-linux-amd64.xqsp")
	if _, _, err := buildBundle(bundlePath, testManifest(), binPath, "xqs-vnc", nil, priv); err != nil {
		t.Fatalf("buildBundle: %v", err)
	}

	tamperZipEntry(t, bundlePath, "xqs-vnc", []byte("tampered binary contents!!"))

	err = verifyBundle(bundlePath, pub)
	if err == nil {
		t.Fatal("expected verifyBundle to fail on a bundle with a tampered file, got nil")
	}
}

// TestVerifyBundle_WrongSignature proves that a bundle whose signature does
// not match the public key (or was produced by a different key) fails
// Ed25519 verification, even though checksums still match.
func TestVerifyBundle_WrongSignature(t *testing.T) {
	dir := t.TempDir()
	binPath := writeFakeBinary(t, dir, "xqs-vnc")

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	bundlePath := filepath.Join(dir, "xqs-vnc-linux-amd64.xqsp")
	if _, _, err := buildBundle(bundlePath, testManifest(), binPath, "xqs-vnc", nil, priv); err != nil {
		t.Fatalf("buildBundle: %v", err)
	}

	// Verify with an unrelated public key: signature won't match.
	if err := verifyBundle(bundlePath, otherPub); err == nil {
		t.Fatal("expected verifyBundle to fail with mismatched public key, got nil")
	}
}

// TestVerifyBundle_TamperedSignatureField proves that corrupting the
// signature field itself (leaving checksums intact) is detected.
func TestVerifyBundle_TamperedSignatureField(t *testing.T) {
	dir := t.TempDir()
	binPath := writeFakeBinary(t, dir, "xqs-vnc")

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	bundlePath := filepath.Join(dir, "xqs-vnc-linux-amd64.xqsp")
	if _, _, err := buildBundle(bundlePath, testManifest(), binPath, "xqs-vnc", nil, priv); err != nil {
		t.Fatalf("buildBundle: %v", err)
	}

	// Replace plugin.json's signature with a well-formed but wrong base64 sig,
	// keeping checksums file untouched -> SHA256SUMS check passes for
	// everything else but the recomputed unsigned plugin.json content is
	// identical (signature is excluded from checksum), so this specifically
	// exercises the Ed25519 verify failure path.
	badSig := make([]byte, ed25519.SignatureSize)
	for i := range badSig {
		badSig[i] = byte(i)
	}
	replacePluginJSONSignature(t, bundlePath, badSig)

	if err := verifyBundle(bundlePath, pub); err == nil {
		t.Fatal("expected verifyBundle to fail with a tampered signature field, got nil")
	}
}

// TestCanonicalJSON_Deterministic proves canonicalJSON produces
// byte-identical output for the same logical input across repeated calls,
// and that key ordering does not affect the result (sorted keys).
func TestCanonicalJSON_Deterministic(t *testing.T) {
	a := map[string]any{
		"b": 1,
		"a": map[string]any{
			"z": 1,
			"y": 2,
		},
		"c": []any{3, 1, 2},
	}
	b := map[string]any{
		"c": []any{3, 1, 2},
		"a": map[string]any{
			"y": 2,
			"z": 1,
		},
		"b": 1,
	}

	out1, err := canonicalJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := canonicalJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(out1) != string(out2) {
		t.Fatalf("canonicalJSON not deterministic across differing key insertion order:\n%s\nvs\n%s", out1, out2)
	}

	// Calling again on the same input must reproduce identical bytes.
	out3, err := canonicalJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(out1) != string(out3) {
		t.Fatalf("canonicalJSON not repeatable:\n%s\nvs\n%s", out1, out3)
	}

	want := `{"a":{"y":2,"z":1},"b":1,"c":[3,1,2]}`
	if string(out1) != want {
		t.Fatalf("canonicalJSON output = %s, want %s", out1, want)
	}
}

// --- test helpers for tampering with an existing .xqsp zip ---

func tamperZipEntry(t *testing.T, bundlePath, name string, newData []byte) {
	t.Helper()
	entries := readZip(t, bundlePath)
	entries[name] = newData
	rewriteZip(t, bundlePath, entries)
}

func replacePluginJSONSignature(t *testing.T, bundlePath string, sig []byte) {
	t.Helper()
	entries := readZip(t, bundlePath)
	pj := entries["plugin.json"]
	var m map[string]any
	if err := json.Unmarshal(pj, &m); err != nil {
		t.Fatal(err)
	}
	m["signature"] = base64.StdEncoding.EncodeToString(sig)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	entries["plugin.json"] = data
	rewriteZip(t, bundlePath, entries)
}

func readZip(t *testing.T, bundlePath string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = data
	}
	return out
}

func rewriteZip(t *testing.T, bundlePath string, entries map[string][]byte) {
	t.Helper()
	f, err := os.Create(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	// Deterministic order for readability; order doesn't affect the tests.
	for _, name := range []string{"plugin.json", "xqs-vnc", "SHA256SUMS"} {
		data, ok := entries[name]
		if !ok {
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		delete(entries, name)
	}
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}
