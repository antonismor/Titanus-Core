package version

import (
	"encoding/json"
	"fmt"
)

const MaxSchema = 1
const Protocol = 1

type Capabilities struct {
	Format         string `json:"format"`
	Build          Build  `json:"build"`
	ProtocolMin    int    `json:"protocol_min"`
	ProtocolMax    int    `json:"protocol_max"`
	ReadSchemaMin  int    `json:"read_schema_min"`
	ReadSchemaMax  int    `json:"read_schema_max"`
	WriteSchemaMin int    `json:"write_schema_min"`
	WriteSchemaMax int    `json:"write_schema_max"`
}

func Compatible() Capabilities {
	return Capabilities{"titanus-capabilities/v1", Info(), Protocol, Protocol, 0, MaxSchema, 0, MaxSchema}
}
func (c Capabilities) Validate() error {
	if c.Format != "titanus-capabilities/v1" || c.Build.StateProfile != StateProfile || c.Build.OS != "linux" || (c.Build.Arch != "amd64" && c.Build.Arch != "arm64") || c.ProtocolMin < 1 || c.ProtocolMax < c.ProtocolMin || c.ReadSchemaMin < 0 || c.ReadSchemaMax < c.ReadSchemaMin || c.WriteSchemaMin < 0 || c.WriteSchemaMax < c.WriteSchemaMin {
		return fmt.Errorf("invalid protocol/data capabilities")
	}
	return nil
}
func (c Capabilities) Supports(schema int) bool {
	return c.Validate() == nil && c.ProtocolMin <= Protocol && c.ProtocolMax >= Protocol && c.ReadSchemaMin <= schema && c.ReadSchemaMax >= schema && c.WriteSchemaMin <= schema && c.WriteSchemaMax >= schema
}

// Unadvertised binaries are supported only on the unchanged legacy schema.
func Admits(c *Capabilities, schema int) bool {
	if c == nil {
		return schema == 0
	}
	return c.Supports(schema)
}
func PrintCapabilities() { b, _ := json.Marshal(Compatible()); fmt.Println(string(b)) }
