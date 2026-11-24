package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const PublicationPDFResultSchemaV1 = "rin-publication-pdf-result/v1"

// PublicationPDFEdition identifies one immutable reading edition.
// ReadingSHA256 identifies the verified HTML/chapters for a
// rendered edition, or the authoritative snapshot for a PDF-only or legacy
// edition.
type PublicationPDFEdition struct {
	ProjectID      string `json:"projectId"`
	SourceRevision string `json:"sourceRevision"`
	SourceSHA256   string `json:"sourceSha256"`
	ReadingSHA256  string `json:"readingSha256"`
}

type PublicationPDFInput struct {
	PublicationPDFEdition
	ProfileSHA256 string `json:"profileSha256"`
}

// PublicationTagPDFSnapshot contains only the public fields of a name-only tag.
type PublicationTagPDFSnapshot struct {
	Title   string `json:"title"`
	Excerpt string `json:"excerpt,omitempty"`
}

func (snapshot PublicationTagPDFSnapshot) Edition(projectID string) (PublicationPDFEdition, error) {
	if !strings.HasPrefix(projectID, "tag-wiki:") || snapshot.Title == "" ||
		strings.TrimSpace(snapshot.Title) != snapshot.Title || strings.TrimSpace(snapshot.Excerpt) != snapshot.Excerpt ||
		len(snapshot.Title) > 1024 || len(snapshot.Excerpt) > 4096 ||
		!utf8.ValidString(snapshot.Title) || !utf8.ValidString(snapshot.Excerpt) ||
		strings.IndexFunc(snapshot.Title, unicode.IsControl) >= 0 || strings.ContainsRune(snapshot.Excerpt, '\x00') {
		return PublicationPDFEdition{}, errors.New("public tag PDF fields are invalid")
	}
	digest := publicationPDFDigest("rin-publication-tag-snapshot/v1", snapshot.Title, snapshot.Excerpt)
	edition := PublicationPDFEdition{
		ProjectID: projectID, SourceRevision: "snapshot:" + digest, SourceSHA256: digest, ReadingSHA256: digest,
	}
	if _, err := edition.ReadVersion(); err != nil {
		return PublicationPDFEdition{}, err
	}
	return edition, nil
}

// ReadVersion is the URL-safe identity of an edition, independent of the
// profile that generates its PDF.
func (edition PublicationPDFEdition) ReadVersion() (string, error) {
	kind, id, err := publicationPDFWork(edition.ProjectID)
	if err != nil {
		return "", err
	}
	revisionType, revision, found := strings.Cut(edition.SourceRevision, ":")
	gitRevision := revisionType == "git" && publicationPDFCommitPattern.MatchString(revision) && strings.Trim(revision, "0") != ""
	snapshotRevision := revisionType == "snapshot" && validPublicationPDFDigest(revision)
	if !found || !(gitRevision || snapshotRevision) {
		return "", errors.New("publication PDF source revision is invalid")
	}
	if !validPublicationPDFDigest(edition.SourceSHA256) || !validPublicationPDFDigest(edition.ReadingSHA256) {
		return "", errors.New("publication PDF source or reading digest is invalid")
	}
	return "v1-" + publicationPDFDigest("rin-publication-pdf-read/v1", kind, id,
		edition.SourceRevision, edition.SourceSHA256, edition.ReadingSHA256), nil
}

// Identity gives the stable reading version and idempotency key for admission.
func (input PublicationPDFInput) Identity() (string, string, error) {
	version, err := input.ReadVersion()
	if err != nil {
		return "", "", err
	}
	if !validPublicationPDFDigest(input.ProfileSHA256) {
		return "", "", errors.New("publication PDF profile digest is invalid")
	}
	return version, publicationPDFDigest("rin-publication-pdf-job/v1", input.ProjectID, version, input.ProfileSHA256), nil
}

var publicationPDFIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
var publicationPDFCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func publicationPDFWork(projectID string) (string, string, error) {
	kind, id, found := strings.Cut(projectID, ":")
	if !found || !publicationPDFIDPattern.MatchString(id) {
		return "", "", errors.New("publication PDF project identity is invalid")
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return "", "", errors.New("publication PDF project ID exceeds content ID range")
	}
	switch kind {
	case "article", "book":
		return kind, id, nil
	case "pdf":
		return "book", id, nil
	case "tag-wiki":
		return "tag", id, nil
	default:
		return "", "", errors.New("publication PDF project type is invalid")
	}
}

func validPublicationPDFDigest(digest string) bool {
	return isSHA256(digest) && strings.Trim(digest, "0") != ""
}

func publicationPDFDigest(fields ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(sum[:])
}

// PublicationPDFResult is the producer's verification receipt. Storage must
// independently check the downloaded bytes and record their durable location
// before the product may mark the edition ready.
type PublicationPDFResult struct {
	SchemaVersion string              `json:"schemaVersion"`
	Input         PublicationPDFInput `json:"input"`
	JobKey        string              `json:"jobKey"`
	ReadVersion   string              `json:"readVersion"`
	SHA256        string              `json:"sha256"`
	Bytes         int64               `json:"bytes"`
	Pages         int                 `json:"pages"`
	MediaType     string              `json:"mediaType"`
}

func (result PublicationPDFResult) ValidateFor(input PublicationPDFInput) error {
	version, key, err := input.Identity()
	if err != nil {
		return err
	}
	if result.SchemaVersion != PublicationPDFResultSchemaV1 || result.Input != input ||
		result.ReadVersion != version || result.JobKey != key {
		return errors.New("publication PDF result does not match the admitted job and reading edition")
	}
	if result.MediaType != "application/pdf" || !validPublicationPDFDigest(result.SHA256) ||
		result.Bytes <= 0 || result.Pages <= 0 {
		return errors.New("publication PDF result has invalid artifact metadata")
	}
	return nil
}
