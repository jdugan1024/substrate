package brain

import (
	"testing"
)

func TestSchemaRegistry_AllSchemasLoad(t *testing.T) {
	expected := []string{
		"note.thought",
		"note.unstructured",
		"crm.contact",
		"crm.interaction",
		"maintenance.task",
		"jobhunt.application",
		"note.link",
	}
	for _, rt := range expected {
		se, err := SchemaFor(rt, "1.0.0")
		if err != nil {
			t.Errorf("SchemaFor(%q, 1.0.0): %v", rt, err)
			continue
		}
		if se.RecordType != rt {
			t.Errorf("SchemaFor(%q): got RecordType %q", rt, se.RecordType)
		}
		if se.SchemaVersion != "1.0.0" {
			t.Errorf("SchemaFor(%q): got SchemaVersion %q", rt, se.SchemaVersion)
		}
		if len(se.Raw) == 0 {
			t.Errorf("SchemaFor(%q): empty Raw bytes", rt)
		}
		if se.Schema == nil {
			t.Errorf("SchemaFor(%q): nil parsed schema", rt)
		}
	}
}

func TestSchemaRegistry_UnknownTypeReturnsError(t *testing.T) {
	_, err := SchemaFor("nonexistent.type", "1.0.0")
	if err == nil {
		t.Error("expected error for unknown type, got nil")
	}
}

func TestSchemaRegistry_KnownRecordTypes(t *testing.T) {
	types := KnownRecordTypes()
	if len(types) < 7 {
		t.Errorf("expected at least 7 known record types, got %d", len(types))
	}
}
