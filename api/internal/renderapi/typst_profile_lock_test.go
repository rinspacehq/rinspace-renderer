package renderapi

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstadapter"
)

// A self-consistent JSON lock is insufficient: its advertised profile must
// also match the runtime that will execute it. Keep this in the ordinary Go
// suite so changing the adapter cannot leave a stale toolchain/cache identity.
func TestLockedTypstProfileMatchesRuntime(t *testing.T) {
	type profile struct {
		ID, SchemaVersion, CompilerVersion, CompilerHash, FontHash, PackageSetDigest string
		AdapterVersion, FinalOutputVersion, MathPresentation                         string
		MathJaxVersion, MathJaxFontVersion                                           string
	}
	body, err := os.ReadFile("../../../deploy/typst-html-toolchain.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Compiler struct{ Version, BinarySHA256 string }
		Font     struct{ SHA256 string }
		Packages struct{ PackageSetDigest string }
		Profile  profile
	}
	if err := json.Unmarshal(body, &lock); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile("../../../deploy/typst.candidate.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var candidate struct{ ProductionProfile profile }
	if err := json.Unmarshal(body, &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.ProductionProfile != lock.Profile {
		t.Fatal("candidate and toolchain locks advertise different Typst profiles")
	}
	if lock.Profile.AdapterVersion != typstadapter.AdapterVersion {
		t.Fatalf("toolchain adapter %q differs from runtime %q", lock.Profile.AdapterVersion, typstadapter.AdapterVersion)
	}
	if lock.Profile.SchemaVersion != typstadapter.ProfileSchemaVersion || lock.Profile.FinalOutputVersion != finaloutput.ContractVersion || lock.Profile.MathPresentation != typstadapter.MathPresentationMathJax {
		t.Fatal("toolchain profile schema, final output or default presentation differs from runtime")
	}
	if lock.Profile.CompilerVersion != lock.Compiler.Version || lock.Profile.CompilerHash != lock.Compiler.BinarySHA256 || lock.Profile.FontHash != lock.Font.SHA256 || lock.Profile.PackageSetDigest != lock.Packages.PackageSetDigest {
		t.Fatal("toolchain profile does not bind its actual compiler, font and package pins")
	}
	if lock.Profile.MathJaxVersion != defaultMathJaxVersion || lock.Profile.MathJaxFontVersion != defaultMathJaxFontVersion {
		t.Fatal("toolchain MathJax profile differs from runtime defaults")
	}
	cfg := Config{
		TypstBin: "/pinned/typst", TypstFontPath: "/pinned/font.ttc",
		TypstVersion: lock.Compiler.Version, TypstBinHash: lock.Compiler.BinarySHA256,
		TypstFontHash: lock.Font.SHA256, TypstPackageHash: lock.Packages.PackageSetDigest,
		TypstTimeout: time.Minute, TypstMaxOutputBytes: 32 << 20, TypstMathRender: true,
		MathJaxVersion: defaultMathJaxVersion, MathJaxFontVersion: defaultMathJaxFontVersion,
	}
	if actual := TypstHTMLProfileID(cfg); actual == "" || actual != lock.Profile.ID {
		t.Fatalf("toolchain advertises profile %q but runtime derives %q", lock.Profile.ID, actual)
	}
}
