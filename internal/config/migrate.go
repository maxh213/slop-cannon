package config

import "fmt"

// migration upgrades a decoded document from schema version To-1 to To. It
// edits the raw map in place; keys it does not understand are left alone
// and rejected later by the strict decode.
type migration struct {
	// To is the schema version the step produces.
	To int
	// Apply performs the upgrade.
	Apply func(doc map[string]any) error
}

// migrations are applied in ascending order of To. Version 1 is the first
// schema, so the list is empty until a version 2 exists; a new step is
// appended here together with a bump of SchemaVersion.
var migrations []migration

// migrate upgrades doc from version `from` to schemaVersion by applying every
// step in sequence and stamping the new schema_version. It fails when a step
// is missing, so a file can never be half-migrated silently.
func migrate(doc map[string]any, from int) error {
	for v := from + 1; v <= schemaVersion; v++ {
		step, ok := findMigration(v)
		if !ok {
			return fmt.Errorf("schema_version %d: no migration to version %d exists", from, v)
		}
		if err := step.Apply(doc); err != nil {
			return fmt.Errorf("migrate config from schema %d to %d: %w", v-1, v, err)
		}
	}
	doc["schema_version"] = int64(schemaVersion)
	return nil
}

func findMigration(to int) (migration, bool) {
	for _, m := range migrations {
		if m.To == to {
			return m, true
		}
	}
	return migration{}, false
}
