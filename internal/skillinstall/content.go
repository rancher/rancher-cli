package skillinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

const (
	// OwnerKey/OwnerValue form the ownership contract: an installation is
	// ours iff metadata carries this EXACT pair. /v1 versions the contract.
	OwnerKey   = "rancherCliInstaller"
	OwnerValue = "skill-install/v1"

	skillFile = "SKILL.md"

	// OwnerMarkerFile is a rendered tree member (digested like any other
	// file): one line of OwnerValue. Staging residue classification reads
	// THIS file — complete-or-absent by construction when written via
	// temp+rename — never the truncatable SKILL.md frontmatter.
	OwnerMarkerFile = ".rancher-skill-owner"

	// digestSentinel replaces the digest value in the canonical view so the
	// digest never hashes bytes containing itself. Fixed forever — part of
	// the digest contract.
	digestSentinel = "digest-pending-0000000000000000000000000000000000000000000000000000000000000000"
)

// ErrNonRegular marks a tree entry that is not a regular file — symlinks,
// FIFOs, and devices are rejected rather than read.
var ErrNonRegular = errors.New("tree contains a non-regular file")

var source fs.FS

// SetSource injects the embedded skill tree. main owns the go:embed
// directive (embed paths cannot climb out of the package directory).
func SetSource(fsys fs.FS) { source = fsys }

// Provenance is the metadata block of an installed SKILL.md.
type Provenance struct {
	Owner         string `yaml:"rancherCliInstaller"`
	CLIVersion    string `yaml:"cliVersion"`
	ContentDigest string `yaml:"contentDigest"`
}

func splitSkillMD(b []byte) (fm []byte, body []byte, err error) {
	rest, found := bytes.CutPrefix(b, []byte("---\n"))
	if !found {
		return nil, nil, fmt.Errorf("SKILL.md: missing frontmatter open")
	}
	fm, body, found = bytes.Cut(rest, []byte("\n---\n"))
	if !found {
		return nil, nil, fmt.Errorf("SKILL.md: missing frontmatter close")
	}
	return fm, body, nil
}

func loadSource() (map[string][]byte, error) {
	if source == nil {
		return nil, fmt.Errorf("skillinstall: source not set")
	}
	files := map[string][]byte{}
	err := fs.WalkDir(source, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s", ErrNonRegular, path)
		}
		b, err := fs.ReadFile(source, path)
		if err != nil {
			return err
		}
		files[path] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := files[skillFile]; !ok {
		return nil, fmt.Errorf("skill source: no %s", skillFile)
	}
	return files, nil
}

// ReadTree loads an installed tree for digesting. Every entry must be a
// regular file per its Lstat type — WalkDir does not follow directory
// symlinks, and this check refuses file symlinks BEFORE any read, so a
// link pointing outside the installation is never opened.
func ReadTree(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s", ErrNonRegular, path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// stampSkillMD appends the metadata block to the original frontmatter
// TEXTUALLY — name/description bytes stay untouched (no YAML re-marshal of
// the whole document, which would reflow the description).
func stampSkillMD(orig []byte, cliVersion, digest string) ([]byte, error) {
	fm, body, err := splitSkillMD(orig)
	if err != nil {
		return nil, err
	}
	meta := map[string]map[string]string{"metadata": {
		OwnerKey:        OwnerValue,
		"cliVersion":    cliVersion,
		"contentDigest": digest,
	}}
	mb, err := yaml.Marshal(meta)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(bytes.TrimRight(fm, "\n"))
	out.WriteString("\n")
	out.Write(mb)
	out.WriteString("---\n")
	out.Write(body)
	return out.Bytes(), nil
}

// Rendered builds the post-transform tree, digest included. Two passes:
// stamp with the sentinel, digest that canonical view, then stamp for real.
// The tree includes the owner marker file: a one-line, rename-atomic
// ownership signal that staging (and installed trees) carry as a first-
// class digested member — sweep classification never depends on parsing a
// possibly-truncated SKILL.md.
func Rendered(cliVersion string) (map[string][]byte, error) {
	files, err := loadSource()
	if err != nil {
		return nil, err
	}
	files[OwnerMarkerFile] = []byte(OwnerValue + "\n")
	canonical, err := stampSkillMD(files[skillFile], cliVersion, digestSentinel)
	if err != nil {
		return nil, err
	}
	view := map[string][]byte{}
	for k, v := range files {
		view[k] = v
	}
	view[skillFile] = canonical
	digest, err := DigestTree(view)
	if err != nil {
		return nil, err
	}
	final, err := stampSkillMD(files[skillFile], cliVersion, digest)
	if err != nil {
		return nil, err
	}
	files[skillFile] = final
	return files, nil
}

// DigestTree hashes the canonical view: SKILL.md participates with its
// contentDigest value replaced by the sentinel; framing is NUL-terminated
// relative path, 8-byte big-endian length, then bytes; sorted path order.
func DigestTree(files map[string][]byte) (string, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		b := files[p]
		if p == skillFile {
			if canon, ok := replaceDigestValue(b, digestSentinel); ok {
				b = canon
			}
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// replaceDigestValue swaps the contentDigest scalar for repl; ok=false when
// no stamped digest exists (pre-render canonical view hashes as-is).
func replaceDigestValue(skillMD []byte, repl string) ([]byte, bool) {
	prov, ok := ReadProvenance(skillMD)
	if !ok || prov.ContentDigest == "" {
		return skillMD, false
	}
	return bytes.Replace(skillMD, []byte(prov.ContentDigest), []byte(repl), 1), true
}

// ReadProvenance parses the metadata block of an installed SKILL.md.
// ok=false means no parseable frontmatter metadata — a foreign file.
func ReadProvenance(skillMD []byte) (Provenance, bool) {
	fm, _, err := splitSkillMD(skillMD)
	if err != nil {
		return Provenance{}, false
	}
	var doc struct {
		Metadata Provenance `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(fm, &doc); err != nil {
		return Provenance{}, false
	}
	if doc.Metadata.Owner == "" {
		return Provenance{}, false
	}
	return doc.Metadata, true
}
