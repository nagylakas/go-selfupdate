package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// manifestSigningContext domain-separates manifest signatures from any other
// use of the same key.
const manifestSigningContext = "go-selfupdate/manifest/v1\n"

const (
	// DefaultMaxManifestBytes is the largest accepted latest.json document.
	DefaultMaxManifestBytes = 1 << 20
	// DefaultMaxDownloadBytes is the largest accepted artifact download when
	// ApplyOptions.MaxDownloadBytes is zero.
	DefaultMaxDownloadBytes = 512 << 20
)

var (
	// ErrIntegrity is wrapped by every error meaning the update itself is
	// untrustworthy, as opposed to a network or configuration problem.
	ErrIntegrity = errors.New("update integrity check failed")
	// ErrUnsigned means a signature was required but the manifest has none.
	ErrUnsigned = fmt.Errorf("%w: update manifest is not signed", ErrIntegrity)
	// ErrBadSignature means the signature does not verify with any trusted key.
	ErrBadSignature = fmt.Errorf("%w: update manifest signature is invalid", ErrIntegrity)
	// ErrInvalidManifest means the manifest is malformed or incomplete.
	ErrInvalidManifest = fmt.Errorf("%w: update manifest is invalid", ErrIntegrity)
	// ErrChecksumMissing means a checksum was required but the artifact has none.
	ErrChecksumMissing = fmt.Errorf("%w: update artifact has no sha256 checksum", ErrIntegrity)
	// ErrChecksumMismatch means the downloaded artifact differs from its checksum.
	ErrChecksumMismatch = fmt.Errorf("%w: update artifact checksum mismatch", ErrIntegrity)
	// ErrTooLarge means a download exceeded its size limit.
	ErrTooLarge = fmt.Errorf("%w: update download exceeds the size limit", ErrIntegrity)
)

// ParsePublicKey decodes a base64 (standard encoding) ed25519 public key.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("not a base64 ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

// EncodePublicKey returns the base64 form accepted by ParsePublicKey.
func EncodePublicKey(key ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key)
}

// SHA256Hex returns the lowercase hex SHA-256 digest of data, the format of
// Artifact.SHA256.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SignManifest validates the manifest, requires a SHA-256 checksum on every
// artifact and sets Signature. The signature covers a canonical encoding of
// the version, build metadata and artifacts (see signedPayload), not the file
// bytes, so whitespace or line-ending changes to latest.json do not break it.
func SignManifest(m *Manifest, key ed25519.PrivateKey) error {
	if len(key) != ed25519.PrivateKeySize {
		return errors.New("invalid ed25519 private key")
	}
	if err := validateSignedFields(m); err != nil {
		return err
	}
	payload, err := signedPayload(m)
	if err != nil {
		return err
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))
	return nil
}

// VerifyManifest checks that the manifest is complete, every artifact has a
// SHA-256 checksum and the signature verifies with one of keys.
func VerifyManifest(m *Manifest, keys []ed25519.PublicKey) error {
	if err := validateSignedFields(m); err != nil {
		return err
	}
	if strings.TrimSpace(m.Signature) == "" {
		return ErrUnsigned
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(m.Signature))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	payload, err := signedPayload(m)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, payload, sig) {
			return nil
		}
	}
	return ErrBadSignature
}

// ParseManifest decodes a latest.json document. With keys, the manifest must
// be signed by one of them (see VerifyManifest); without keys it is only
// validated, exactly like FetchManifest.
func ParseManifest(data []byte, keys []ed25519.PublicKey) (*Manifest, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	var payload manifestPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	manifest := &Manifest{
		Version:      payload.Version,
		SourceCommit: payload.SourceCommit,
		BuildTimeUTC: payload.BuildTimeUTC,
		Artifacts:    payload.Artifacts,
		Signature:    payload.Signature,
	}
	if len(keys) > 0 {
		// Signed manifests must use the artifact list; the unsigned legacy
		// linux_archive field is ignored.
		if err := VerifyManifest(manifest, keys); err != nil {
			return nil, err
		}
		return manifest, nil
	}

	if manifest.Version == "" {
		return nil, errors.New("update manifest is missing a version")
	}
	if len(manifest.Artifacts) == 0 && strings.TrimSpace(payload.LegacyLinuxFile) != "" {
		manifest.Artifacts = []Artifact{{Name: "linux", URL: payload.LegacyLinuxFile}}
	}
	if len(manifest.Artifacts) == 0 {
		return nil, errors.New("update manifest is missing artifacts")
	}
	if err := validateArtifacts(manifest.Artifacts); err != nil {
		return nil, err
	}
	for _, a := range manifest.Artifacts {
		if a.SHA256 != "" && !validSHA256(a.SHA256) {
			return nil, fmt.Errorf("%w: artifact %q has a malformed sha256", ErrInvalidManifest, a.Name)
		}
	}
	return manifest, nil
}

func validateSignedFields(m *Manifest) error {
	if m == nil || strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("%w: missing version", ErrInvalidManifest)
	}
	if len(m.Artifacts) == 0 {
		return fmt.Errorf("%w: missing artifacts", ErrInvalidManifest)
	}
	if err := validateArtifacts(m.Artifacts); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	for _, a := range m.Artifacts {
		if !validSHA256(a.SHA256) {
			return fmt.Errorf("%w: artifact %q has no valid sha256", ErrInvalidManifest, a.Name)
		}
	}
	return nil
}

// signedPayload is the exact byte string that is signed: a context prefix and
// the canonical JSON encoding of every field the updater acts on.
func signedPayload(m *Manifest) ([]byte, error) {
	canonical := struct {
		Version      string     `json:"version"`
		SourceCommit string     `json:"source_commit"`
		BuildTimeUTC string     `json:"build_time_utc"`
		Artifacts    []Artifact `json:"artifacts"`
	}{m.Version, m.SourceCommit, m.BuildTimeUTC, m.Artifacts}
	body, err := json.Marshal(canonical)
	if err != nil {
		return nil, err
	}
	return append([]byte(manifestSigningContext), body...), nil
}

// validSHA256 accepts exactly 64 lowercase hex characters, so the signed
// canonical form has a single spelling.
func validSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// checksumMatches compares a computed digest with the expected one in
// constant time.
func checksumMatches(got []byte, want string) bool {
	return validSHA256(want) && subtle.ConstantTimeCompare([]byte(hex.EncodeToString(got)), []byte(want)) == 1
}
