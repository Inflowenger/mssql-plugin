package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Inflowenger/go-plugin-sdk/formkit"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// metaTimeout keeps meta RPCs snappy: they answer a form a user is waiting on,
// so a slow server should fail fast rather than hang the drawer.
const metaTimeout = 15 * time.Second

// Metas are synchronous helper RPCs served outside the job lifecycle: the
// settings form's "Test connection" button, and the Insert form's one lookup —
// a single button that lists a connection's tables and then loads the chosen
// table's columns as form fields.
func (r *Registry) Metas() []sdkv1.Meta {
	return []sdkv1.Meta{
		{Method: "mssql.meta.ping.check", RequestHandler: r.metaPingCheck},
		{Method: "mssql.meta.table.pick", RequestHandler: r.metaTablePick},
	}
}

// SQL behind the Insert form's lookup. Both filter by an optional schema — the
// empty string, the default, matches every user schema — and both leave the
// system schemas out so the picker shows only what a person would write to.
const (
	tablesSQL = `SELECT TABLE_SCHEMA AS table_schema, TABLE_NAME AS table_name
FROM INFORMATION_SCHEMA.TABLES
WHERE TABLE_TYPE = 'BASE TABLE'
  AND TABLE_SCHEMA NOT IN ('sys', 'INFORMATION_SCHEMA')
  AND (@p1 = '' OR TABLE_SCHEMA = @p1)
ORDER BY TABLE_SCHEMA, TABLE_NAME`

	columnsSQL = `SELECT COLUMN_NAME AS column_name, DATA_TYPE AS data_type
FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_NAME = @p1
  AND (@p2 = '' OR TABLE_SCHEMA = @p2)
ORDER BY ORDINAL_POSITION`
)

// metaTablePick backs the Insert form's single "Tables / columns" button, so the
// form needs no throwaway field just to anchor a second button. It reads what
// the Table box holds (posted as `value`) and branches:
//
//   - empty box → list the connection's tables, rebuilding Table as a drop-down;
//   - a table set → load that table's columns and re-render the form with a value
//     field per column (see columnsEnvelope).
//
// A name that matches no table falls through to the list, so a typo just shows
// the choices. Either branch is a whole-form answer, not a patch, because the
// options and the column fields did not exist when the plugin was compiled.
func (r *Registry) metaTablePick(req sdkv1.Request) any {
	call := decodeMeta[map[string]any](req.Data)
	conn, _ := call["settings"].(map[string]any)
	client, err := r.pool.Client(conn)
	if err != nil {
		return formkit.Failure("%s", err).Patch(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()

	schema := metaString(call, "schema")
	table := strings.TrimSpace(metaString(call, "value"))
	if table == "" {
		table = strings.TrimSpace(metaString(call, "table"))
	}

	// A table already chosen: load its columns and re-render with a value field
	// for each. A name matching nothing falls through to the table list below.
	if table != "" {
		res, err := client.Query(ctx, columnsSQL, []any{table, schema}, 0)
		if err != nil {
			return formkit.Failure("cannot read columns of %s: %s", table, err).About("table").Patch(nil)
		}
		if len(res.Rows) > 0 {
			return columnsEnvelope(res.Rows, formkit.FormData(call))
		}
	}

	res, err := client.Query(ctx, tablesSQL, []any{schema}, 0)
	if err != nil {
		return formkit.Failure("cannot list tables: %s", err).Patch(nil)
	}
	options := make([]formkit.Option, 0, len(res.Rows))
	for _, row := range res.Rows {
		name, _ := row["table_name"].(string)
		sch, _ := row["table_schema"].(string)
		options = append(options, formkit.Option{Value: name, Label: sch + "." + name})
	}
	if len(options) == 0 {
		return formkit.Warning("no tables found on %s — check the schema", client.ConnInfo()).About("table").Patch(nil)
	}

	heading := formkit.Success("%d tables on %s — pick one, then press ↻ again to load its columns", len(options), client.ConnInfo())
	if table != "" {
		heading = formkit.Warning("no table %q on %s — pick one below, then press ↻ again for its columns", table, client.ConnInfo())
	}
	return formkit.Choose(insertForm, "table", options, formkit.FormData(call), heading)
}

// columnsEnvelope rebuilds the Insert dialog with a value field per column. It
// starts from the form's own schema and layout (a fresh copy from the builder),
// adds a `values` object whose properties are the columns, and drops a control
// for each into a "Column values" group placed just above the JSON-record field.
// The returned map carries only envelope keys, which is what tells the host to
// re-render the dialog rather than patch a single field.
func columnsEnvelope(rows []map[string]any, data map[string]any) map[string]any {
	schema := insertFormDef.SchemaMap()
	ui := insertFormDef.UIMap()

	valueProps := make(map[string]any, len(rows))
	controls := make([]any, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		col, _ := row["column_name"].(string)
		if col == "" || seen[col] {
			continue
		}
		seen[col] = true
		dataType, _ := row["data_type"].(string)
		valueProps[col] = map[string]any{
			"type":        "string",
			"title":       col,
			"description": strings.TrimSpace(dataType + " — a literal or a {{$.path}} token; blank to use the default"),
		}
		controls = append(controls, map[string]any{
			"type":  "Control",
			"scope": "#/properties/values/properties/" + col,
		})
	}

	if props, ok := schema["properties"].(map[string]any); ok {
		props["values"] = map[string]any{
			"type":       "object",
			"title":      "Column values",
			"properties": valueProps,
		}
	}

	group := map[string]any{"type": "Group", "label": "Column values", "elements": controls}
	if elements, ok := ui["elements"].([]any); ok {
		ui["elements"] = insertBefore(elements, "#/properties/record", group)
	}

	return map[string]any{
		"schema":         schema,
		"uischema":       ui,
		"data":           data,
		formkit.NotifKey: formkit.Success("Loaded %d columns — fill the ones you want", len(controls)).About("table"),
	}
}

// insertBefore splices an element into a UI Schema's element list just ahead of
// the control at the given scope, appending it if that control is not found.
func insertBefore(elements []any, scope string, element any) []any {
	out := make([]any, 0, len(elements)+1)
	inserted := false
	for _, el := range elements {
		if !inserted {
			if m, ok := el.(map[string]any); ok {
				if s, _ := m["scope"].(string); s == scope {
					out = append(out, element)
					inserted = true
				}
			}
		}
		out = append(out, el)
	}
	if !inserted {
		out = append(out, element)
	}
	return out
}

// metaString reads a string field from a meta call's flat body, or "" if it is
// absent or not a string.
func metaString(call map[string]any, key string) string {
	if v, ok := call[key].(string); ok {
		return v
	}
	return ""
}

// metaPingCheck backs the settings form's "Test connection" button and its
// submit validation. It opens the connection being edited and pings it, so a
// wrong host or password is reported in the dialog instead of failing every
// node later.
//
// The values arrive one of two ways: on an action's drawer the bound profile is
// under "settings"; the plugin set-up dialog has no bound profile and sends the
// values being edited at the top level, alongside the keys the host adds.
func (r *Registry) metaPingCheck(req sdkv1.Request) any {
	body := decodeMeta[map[string]any](req.Data)

	conn, _ := body["settings"].(map[string]any)
	if len(conn) == 0 {
		conn = body
		for _, hostKey := range []string{"settings", "value", "targetField", "form"} {
			delete(conn, hostKey)
		}
	}

	client, err := r.pool.Client(conn)
	if err != nil {
		return formkit.Failure("%s", err).Patch(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		return formkit.Failure("cannot reach %s: %s", client.ConnInfo(), err).Patch(nil)
	}
	// A patch with nothing but a message: the test writes no value into the
	// profile, and saying what it found is the whole point of the button.
	return formkit.Success("Connected to %s.", client.ConnInfo()).Patch(nil)
}

// Settings declares what a SQL Server settings profile must contain. The
// platform renders this form when the plugin is set up, stores the answers as a
// named, reusable profile, and ships the values back with every call as
// body.settings.
//
// The submit handler is a validator, not a store: it checks the values against
// the live server so a wrong connection is reported in the dialog.
func (r *Registry) Settings() *sdkv1.Settings {
	return &sdkv1.Settings{
		FormBuilder: settingsForm,
		SubmitHandler: func(req sdkv1.Request) sdkv1.Response {
			submitted := decodeMeta[map[string]any](req.Data)
			if nested, ok := submitted["settings"].(map[string]any); ok {
				submitted = nested
			}

			client, err := r.pool.Client(submitted)
			if err != nil {
				return sdkv1.Response{Error: err.Error()}
			}

			ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
			defer cancel()

			if err := client.Ping(ctx); err != nil {
				return sdkv1.Response{Error: "connection test failed: " + err.Error()}
			}
			return sdkv1.Response{Data: map[string]any{"ok": true, "connectedTo": client.ConnInfo()}}
		},
	}
}

// SettingsForm exposes the same form for PluginIntro.Settings — the document the
// platform's plugin set-up dialog reads to build the profile editor.
func (r *Registry) SettingsForm() *sdkv1.FormBuilder {
	settings := r.Settings().FormBuilder
	return &settings
}

// decodeMeta reads a meta RPC's arguments. Meta calls come from the form
// renderer rather than the job pipeline, so the payload may or may not be
// wrapped in the {_registry, body} envelope — try both, and treat anything
// unreadable as "no arguments" rather than an error.
func decodeMeta[T any](data []byte) T {
	var out T
	if len(bytes.TrimSpace(data)) == 0 {
		return out
	}

	var envelope struct {
		Body json.RawMessage `json:"body"`
	}
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Body) > 0 {
		if json.Unmarshal(envelope.Body, &out) == nil {
			return out
		}
	}

	_ = json.Unmarshal(data, &out)
	return out
}
