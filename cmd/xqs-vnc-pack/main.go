// Command xqs-vnc-pack builds release .xqsp bundles for the VNC plugin:
// it cross-compiles cmd/xqs-vnc for each target platform, assembles a
// ZIP bundle (plugin.json + platform binary + ui/), writes SHA256SUMS,
// and Ed25519-signs the manifest per docs/plugin-manifest.md's
// "Signature" and "Bundle format" sections.
//
// Format interpretation (docs/plugin-manifest.md is not fully explicit
// on every point; see task-14-report.md for the full rationale):
//
//   - `.xqsp` is a ZIP archive (doc says this directly).
//   - engine.entry is a single filename, and the host "must exist and
//     match host GOOS at discovery/install" — so each bundle targets a
//     single platform, with entry rewritten per-platform (xqs-vnc.exe
//     on Windows, xqs-vnc elsewhere). One .xqsp is produced per
//     platform: dist/xqs-vnc-<os>-<arch>.xqsp.
//   - SHA256SUMS covers "all files except the checksums file itself" —
//     applied here to the *unsigned* canonical plugin.json, the
//     platform binary, and every file under ui/. Using the unsigned
//     manifest bytes for the plugin.json entry avoids a chicken/egg
//     loop where signing plugin.json would change plugin.json's own
//     checksum after SHA256SUMS was already hashed into the signature
//     envelope; a verifier that (as it must, to check the signature)
//     re-derives the unsigned manifest by stripping `signature` before
//     hashing will get a matching SHA256SUMS entry.
//   - The signature is embedded in plugin.json's `signature` field
//     (base64 Ed25519), per the doc's envelope: sign
//     {"manifest": <plugin.json without "signature">, "checksumsSha256": <hex sha256 of SHA256SUMS bytes, CRLF normalized to LF>}
//     with map keys sorted (canonical JSON).
package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type target struct {
	goos, goarch string
}

func (t target) entryName() string {
	if t.goos == "windows" {
		return "xqs-vnc.exe"
	}
	return "xqs-vnc"
}

func (t target) bundleName() string {
	return fmt.Sprintf("xqs-vnc-%s-%s.xqsp", t.goos, t.goarch)
}

var defaultTargets = []target{
	{"windows", "amd64"},
	{"linux", "amd64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
}

func main() {
	var (
		repoRoot  = flag.String("repo", ".", "repository root (contains go.mod, plugin.json, ui/)")
		outDir    = flag.String("out", "dist", "output directory for build artifacts and bundles")
		keyPath   = flag.String("key", "", "path to Ed25519 private key seed (32 raw bytes); generated at <out>/dev-signing-key if absent")
		targetsCS = flag.String("targets", "", "comma-separated goos/goarch pairs, e.g. windows/amd64,linux/amd64 (default: all)")
	)
	flag.Parse()

	root, err := filepath.Abs(*repoRoot)
	must(err)
	out := filepath.Join(root, *outDir)
	must(os.MkdirAll(out, 0o755))

	targets := defaultTargets
	if *targetsCS != "" {
		targets = nil
		for _, part := range strings.Split(*targetsCS, ",") {
			p := strings.SplitN(strings.TrimSpace(part), "/", 2)
			if len(p) != 2 {
				fatalf("invalid target %q, want goos/goarch", part)
			}
			targets = append(targets, target{p[0], p[1]})
		}
	}

	priv := loadOrGenerateKey(*keyPath, out)
	pub := priv.Public().(ed25519.PublicKey)
	must(os.WriteFile(filepath.Join(out, "dev-signing-key.pub"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644))

	manifestBytes, err := os.ReadFile(filepath.Join(root, "plugin.json"))
	must(err)
	var manifest map[string]any
	must(json.Unmarshal(manifestBytes, &manifest))
	delete(manifest, "signature") // never trust a pre-existing signature field as input

	uiRoot := filepath.Join(root, "ui")
	uiFiles, err := collectFiles(uiRoot, "ui")
	must(err)

	for _, t := range targets {
		fmt.Printf("== %s/%s ==\n", t.goos, t.goarch)
		binPath := filepath.Join(out, "bin", fmt.Sprintf("%s-%s", t.goos, t.goarch), t.entryName())
		must(crossCompile(root, t, binPath))
		info, err := os.Stat(binPath)
		must(err)
		if info.Size() == 0 {
			fatalf("compiled binary %s is empty", binPath)
		}
		fmt.Printf("  binary: %s (%d bytes)\n", binPath, info.Size())

		platformManifest := cloneManifest(manifest)
		engine, _ := platformManifest["engine"].(map[string]any)
		if engine == nil {
			fatalf("plugin.json missing engine block")
		}
		engine["entry"] = t.entryName()

		bundlePath := filepath.Join(out, t.bundleName())
		sums, sig, err := buildBundle(bundlePath, platformManifest, binPath, t.entryName(), uiFiles, priv)
		must(err)
		fmt.Printf("  bundle: %s\n", bundlePath)
		fmt.Printf("  SHA256SUMS entries: %d\n", len(sums))
		fmt.Printf("  signature (base64): %s\n", sig)

		must(verifyBundle(bundlePath, pub))
		fmt.Printf("  verify: OK\n")
	}

	fmt.Println("done")
}

// bundleFile is one file to place inside the .xqsp archive.
type bundleFile struct {
	arcName string // path inside the zip
	data    []byte
}

func collectFiles(dir, arcPrefix string) ([]bundleFile, error) {
	var files []bundleFile
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, bundleFile{
			arcName: path.Join(arcPrefix, filepath.ToSlash(rel)),
			data:    data,
		})
		return nil
	})
	return files, err
}

func cloneManifest(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	must(err)
	var out map[string]any
	must(json.Unmarshal(b, &out))
	return out
}

func crossCompile(root string, t target, outPath string) error {
	must(os.MkdirAll(filepath.Dir(outPath), 0o755))
	cmd := exec.Command("go", "build", "-o", outPath, "./cmd/xqs-vnc")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GOOS="+t.goos,
		"GOARCH="+t.goarch,
		"CGO_ENABLED=0",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s/%s: %w\n%s", t.goos, t.goarch, err, buf.String())
	}
	return nil
}

// canonicalJSON marshals v with map keys sorted lexicographically at
// every level ("canonical JSON" per docs/plugin-manifest.md's
// Signature section), so the same logical object always produces the
// same bytes.
func canonicalJSON(v any) ([]byte, error) {
	norm := normalize(v)
	return json.Marshal(norm)
}

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(orderedMap, 0, len(keys))
		for _, k := range keys {
			out = append(out, kv{k, normalize(x[k])})
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e)
		}
		return out
	default:
		return v
	}
}

type kv struct {
	K string
	V any
}
type orderedMap []kv

func (o orderedMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(p.K)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(p.V)
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// buildBundle assembles the .xqsp ZIP: unsigned manifest is hashed for
// SHA256SUMS, the manifest+checksum envelope is signed, the signed
// manifest is written into the zip as plugin.json.
func buildBundle(bundlePath string, manifest map[string]any, binPath, binArcName string, uiFiles []bundleFile, priv ed25519.PrivateKey) (map[string]string, string, error) {
	unsignedManifestBytes, err := canonicalJSON(manifest)
	if err != nil {
		return nil, "", err
	}

	binData, err := os.ReadFile(binPath)
	if err != nil {
		return nil, "", err
	}

	files := []bundleFile{
		{arcName: "plugin.json", data: unsignedManifestBytes},
		{arcName: binArcName, data: binData},
	}
	files = append(files, uiFiles...)

	sort.Slice(files, func(i, j int) bool { return files[i].arcName < files[j].arcName })

	sums := make(map[string]string, len(files))
	var sb strings.Builder
	for _, f := range files {
		h := sha256.Sum256(f.data)
		hexSum := hex.EncodeToString(h[:])
		sums[f.arcName] = hexSum
		fmt.Fprintf(&sb, "%s  %s\n", hexSum, f.arcName)
	}
	sumsBytes := []byte(strings.ReplaceAll(sb.String(), "\r\n", "\n"))

	checksumsSha256 := sha256.Sum256(sumsBytes)
	checksumsHex := hex.EncodeToString(checksumsSha256[:])

	envelope := map[string]any{
		"manifest":        manifest,
		"checksumsSha256": checksumsHex,
	}
	envelopeBytes, err := canonicalJSON(envelope)
	if err != nil {
		return nil, "", err
	}

	sig := ed25519.Sign(priv, envelopeBytes)
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	signedManifest := cloneManifest(manifest)
	signedManifest["signature"] = sigB64
	signedManifestBytes, err := json.MarshalIndent(signedManifest, "", "  ")
	if err != nil {
		return nil, "", err
	}

	must(os.MkdirAll(filepath.Dir(bundlePath), 0o755))
	zf, err := os.Create(bundlePath)
	if err != nil {
		return nil, "", err
	}
	defer zf.Close()
	zw := zip.NewWriter(zf)

	writeEntry := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}

	// plugin.json in the archive is the SIGNED manifest (what the host
	// actually loads); its SHA256SUMS entry above was computed from the
	// unsigned form, see package doc.
	if err := writeEntry("plugin.json", signedManifestBytes); err != nil {
		return nil, "", err
	}
	if err := writeEntry(binArcName, binData); err != nil {
		return nil, "", err
	}
	for _, f := range uiFiles {
		if err := writeEntry(f.arcName, f.data); err != nil {
			return nil, "", err
		}
	}
	if err := writeEntry("SHA256SUMS", sumsBytes); err != nil {
		return nil, "", err
	}

	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return sums, sigB64, nil
}

// verifyBundle re-opens the produced .xqsp, recomputes SHA256SUMS from
// the archived files (using the unsigned form of plugin.json, i.e.
// with `signature` stripped, matching how it was hashed at build
// time), and checks the Ed25519 signature against pub. This proves
// the round trip actually works, not just that files got written.
func verifyBundle(bundlePath string, pub ed25519.PublicKey) error {
	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		return err
	}
	defer zr.Close()

	content := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		content[f.Name] = data
	}

	sumsBytes, ok := content["SHA256SUMS"]
	if !ok {
		return fmt.Errorf("bundle missing SHA256SUMS")
	}
	signedManifestBytes, ok := content["plugin.json"]
	if !ok {
		return fmt.Errorf("bundle missing plugin.json")
	}
	var signedManifest map[string]any
	if err := json.Unmarshal(signedManifestBytes, &signedManifest); err != nil {
		return err
	}
	sigB64, _ := signedManifest["signature"].(string)
	if sigB64 == "" {
		return fmt.Errorf("plugin.json has no signature")
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return err
	}
	unsignedManifest := cloneManifest(signedManifest)
	delete(unsignedManifest, "signature")
	unsignedManifestBytes, err := canonicalJSON(unsignedManifest)
	if err != nil {
		return err
	}

	// Recompute SHA256SUMS from archive contents and compare byte-for-byte.
	recomputed := map[string][]byte{}
	for name, data := range content {
		if name == "SHA256SUMS" {
			continue
		}
		if name == "plugin.json" {
			recomputed[name] = unsignedManifestBytes
			continue
		}
		recomputed[name] = data
	}
	names := make([]string, 0, len(recomputed))
	for n := range recomputed {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		h := sha256.Sum256(recomputed[n])
		fmt.Fprintf(&sb, "%s  %s\n", hex.EncodeToString(h[:]), n)
	}
	recomputedSums := []byte(strings.ReplaceAll(sb.String(), "\r\n", "\n"))
	if !bytes.Equal(recomputedSums, bytes.ReplaceAll(sumsBytes, []byte("\r\n"), []byte("\n"))) {
		return fmt.Errorf("SHA256SUMS mismatch:\nwant:\n%s\ngot:\n%s", sumsBytes, recomputedSums)
	}

	checksumsSha256 := sha256.Sum256(bytes.ReplaceAll(sumsBytes, []byte("\r\n"), []byte("\n")))
	envelope := map[string]any{
		"manifest":        unsignedManifest,
		"checksumsSha256": hex.EncodeToString(checksumsSha256[:]),
	}
	envelopeBytes, err := canonicalJSON(envelope)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, envelopeBytes, sig) {
		return fmt.Errorf("ed25519 signature verification FAILED")
	}
	return nil
}

func loadOrGenerateKey(keyPath, out string) ed25519.PrivateKey {
	if keyPath == "" {
		keyPath = filepath.Join(out, "dev-signing-key")
	}
	if data, err := os.ReadFile(keyPath); err == nil {
		seed, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if decErr == nil && len(seed) == ed25519.SeedSize {
			fmt.Printf("using existing dev signing key: %s\n", keyPath)
			return ed25519.NewKeyFromSeed(seed)
		}
		fatalf("existing key at %s is not a valid base64-encoded 32-byte Ed25519 seed", keyPath)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(err)
	seed := priv.Seed()
	must(os.MkdirAll(filepath.Dir(keyPath), 0o755))
	must(os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(seed)+"\n"), 0o600))
	fmt.Printf("generated NEW DEV/TEST-ONLY Ed25519 signing key: %s\n", keyPath)
	fmt.Println("  *** this is a throwaway development key, not a production release signing key ***")
	_ = pub
	return priv
}

func must(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "xqs-vnc-pack: "+format+"\n", args...)
	os.Exit(1)
}
