package jobresult

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

const StoredOutputSchemaVersion = "rin-stored-render-output/v1"

// StoredOutput is private worker storage. The public result route exposes only Result; the exact
// legacy response is retained solely for the authenticated synchronous compatibility adapter.
type StoredOutput struct {
	SchemaVersion string                 `json:"schemaVersion"`
	Result        contracts.RenderResult `json:"result"`
	Compatibility json.RawMessage        `json:"compatibility"`
}

func (output StoredOutput) Validate() error {
	if output.SchemaVersion != StoredOutputSchemaVersion {
		return fmt.Errorf("unsupported stored output schema %q", output.SchemaVersion)
	}
	if err := output.Result.Validate(); err != nil {
		return fmt.Errorf("stored canonical result: %w", err)
	}
	if len(output.Compatibility) == 0 || !json.Valid(output.Compatibility) || output.Compatibility[0] != '{' {
		return errors.New("stored compatibility response must be a JSON object")
	}
	return nil
}

func Decode(body []byte) (StoredOutput, error) {
	var output StoredOutput
	if err := json.Unmarshal(body, &output); err != nil {
		return StoredOutput{}, fmt.Errorf("decode stored renderer output: %w", err)
	}
	if err := output.Validate(); err != nil {
		return StoredOutput{}, err
	}
	return output, nil
}
