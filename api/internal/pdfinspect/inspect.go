package pdfinspect

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

const SchemaVersion = "rin-pdf-inspection/v1"

var (
	titlePattern = regexp.MustCompile(`/Title\s*\(([^()]*)\)`)
	version      = regexp.MustCompile(`^%PDF-([0-9]+\.[0-9]+)`)
)

type TOCEntry struct {
	Title string `json:"title"`
	Page  int    `json:"page,omitempty"`
}

type PageDerivative struct {
	SourcePage int    `json:"sourcePage"`
	MediaType  string `json:"mediaType"`
	Purpose    string `json:"purpose"`
}

type Result struct {
	SchemaVersion     string           `json:"schemaVersion"`
	ContentKind       string           `json:"contentKind"`
	Engine            string           `json:"engine"`
	SourceSHA256      string           `json:"sourceSha256"`
	SourceBytes       int64            `json:"sourceBytes"`
	PDFVersion        string           `json:"pdfVersion"`
	PageCount         int              `json:"pageCount"`
	TOC               []TOCEntry       `json:"toc"`
	Derivatives       []PageDerivative `json:"derivatives"`
	OriginalReencoded bool             `json:"originalReencoded"`
}

var ErrQuarantined = errors.New("PDF contains active or embedded content and was quarantined")

func Inspect(body []byte) (Result, error) {
	if len(body) < 16 || !bytes.HasPrefix(body, []byte("%PDF-")) {
		return Result{}, errors.New("PDF signature is invalid")
	}
	tail := body
	if len(tail) > 2048 {
		tail = tail[len(tail)-2048:]
	}
	if !bytes.Contains(tail, []byte("%%EOF")) {
		return Result{}, errors.New("PDF trailer is missing")
	}
	versionMatch := version.FindSubmatch(body)
	if len(versionMatch) != 2 {
		return Result{}, errors.New("PDF version is invalid")
	}
	configuration := model.NewDefaultConfiguration()
	configuration.ValidationMode = model.ValidationRelaxed
	parsed, parseErr := api.ReadAndValidate(bytes.NewReader(body), configuration)
	if parseErr != nil || parsed == nil || parsed.PageCount <= 0 {
		return Result{}, errors.New("PDF contains no pages")
	}
	if containsUnsafePDFObject(parsed) {
		return Result{}, ErrQuarantined
	}
	pageCount := parsed.PageCount
	toc := make([]TOCEntry, 0)
	for _, match := range titlePattern.FindAllSubmatch(body, 81) {
		title := strings.TrimSpace(string(match[1]))
		if title != "" {
			toc = append(toc, TOCEntry{Title: title})
		}
		if len(toc) == 80 {
			break
		}
	}
	digest := sha256.Sum256(body)
	previewPages := pageCount
	if previewPages > 3 {
		previewPages = 3
	}
	derivatives := []PageDerivative{{SourcePage: 1, MediaType: "image/webp", Purpose: "cover"}}
	for page := 1; page <= previewPages; page++ {
		derivatives = append(derivatives, PageDerivative{SourcePage: page, MediaType: "image/webp", Purpose: "preview-" + strconv.Itoa(page)})
	}
	return Result{
		SchemaVersion: SchemaVersion, ContentKind: "pdf", Engine: "pdf-inspect",
		SourceSHA256: hex.EncodeToString(digest[:]), SourceBytes: int64(len(body)), PDFVersion: string(versionMatch[1]),
		PageCount: pageCount, TOC: toc, Derivatives: derivatives, OriginalReencoded: false,
	}, nil
}

func containsUnsafePDFObject(context *model.Context) bool {
	if context == nil || context.XRefTable == nil {
		return true
	}
	for _, entry := range context.XRefTable.Table {
		if entry != nil && !entry.Free && unsafeObject(entry.Object) {
			return true
		}
	}
	return context.XRefTable.Names["JavaScript"] != nil || context.XRefTable.Names["EmbeddedFiles"] != nil
}

func unsafeObject(object types.Object) bool {
	switch value := object.(type) {
	case types.Dict:
		return unsafeDictionary(value)
	case types.StreamDict:
		return unsafeDictionary(value.Dict)
	case *types.StreamDict:
		return value != nil && unsafeDictionary(value.Dict)
	case types.Array:
		for _, item := range value {
			if unsafeObject(item) {
				return true
			}
		}
	case *types.ObjectStreamDict:
		return value != nil && (unsafeDictionary(value.Dict) || unsafeObject(value.ObjArray))
	case *types.XRefStreamDict:
		return value != nil && unsafeDictionary(value.Dict)
	}
	return false
}

func unsafeDictionary(dictionary types.Dict) bool {
	for key, value := range dictionary {
		switch key {
		case "JavaScript", "JS", "AA", "EmbeddedFiles":
			return true
		}
		if name, ok := value.(types.Name); ok && (key == "S" || key == "Type" || key == "Subtype") {
			switch name.Value() {
			case "JavaScript", "Launch", "EmbeddedFile", "RichMedia":
				return true
			}
		}
		if unsafeObject(value) {
			return true
		}
	}
	return false
}
