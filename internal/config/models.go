package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Modalities a model can accept/produce.
var validModalities = map[string]bool{"text": true, "image": true, "video": true, "audio": true}

// ModelMeta is the per-model metadata stored in the models table: what the
// model accepts/produces, how much context it takes, and which reasoning
// effort levels it offers.
type ModelMeta struct {
	ModelID          string   `json:"model_id"`
	InputModality    []string `json:"input_modality"`
	OutputModality   []string `json:"output_modality"`
	ContextLength    int64    `json:"context_length"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
	ReasoningDefault string   `json:"reasoning_default"`
}

// DefaultModelMeta returns the spec defaults for one model: text-only
// modalities, 128K context, efforts [low, medium, high] with the 2nd
// element ("medium") as the default.
func DefaultModelMeta(id string) ModelMeta {
	return ModelMeta{
		ModelID:          id,
		InputModality:    []string{"text"},
		OutputModality:   []string{"text"},
		ContextLength:    DefaultContextLength,
		ReasoningEfforts: append([]string(nil), DefaultReasoningEfforts...),
		ReasoningDefault: DefaultReasoningEfforts[1],
	}
}

// SanitizeMeta normalizes one model metadata entry in place: modalities are
// filtered to the known set (defaulting to ["text"]), context length must be
// positive (default 128K), effort levels fall back to the default list and
// the default effort must be one of the levels (falls back to the 2nd
// element).
func SanitizeMeta(m *ModelMeta) {
	m.ModelID = strings.TrimSpace(m.ModelID)
	m.InputModality = filterModalities(m.InputModality)
	m.OutputModality = filterModalities(m.OutputModality)
	if m.ContextLength <= 0 {
		m.ContextLength = DefaultContextLength
	}
	m.ReasoningEfforts = filterEmpty(m.ReasoningEfforts)
	if len(m.ReasoningEfforts) == 0 {
		m.ReasoningEfforts = append([]string(nil), DefaultReasoningEfforts...)
	}
	found := false
	for _, e := range m.ReasoningEfforts {
		if e == m.ReasoningDefault {
			found = true
			break
		}
	}
	if !found {
		// Default effort = the 2nd element of the levels list (or the only
		// element when there is just one).
		idx := 1
		if idx >= len(m.ReasoningEfforts) {
			idx = len(m.ReasoningEfforts) - 1
		}
		m.ReasoningDefault = m.ReasoningEfforts[idx]
	}
}

func filterModalities(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.ToLower(strings.TrimSpace(m))
		if validModalities[m] && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return []string{"text"}
	}
	return out
}

func filterEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ModelMetas returns metadata for the given model ids — one entry per id, in
// input order. Ids without a stored row get the defaults applied.
func (s *Store) ModelMetas(ctx context.Context, ids []string) ([]ModelMeta, error) {
	stored := map[string]ModelMeta{}
	if len(ids) > 0 {
		rows, err := s.db.QueryContext(ctx, `
			SELECT model_id, input_modality, output_modality, context_length, reasoning_efforts, reasoning_default
			FROM models WHERE model_id IN (`+placeholders(len(ids))+`)`, args(ids)...)
		if err != nil {
			return nil, fmt.Errorf("query models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanModelMeta(rows)
			if err != nil {
				return nil, err
			}
			stored[m.ModelID] = m
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := make([]ModelMeta, 0, len(ids))
	for _, id := range ids {
		if m, ok := stored[id]; ok {
			out = append(out, m)
		} else {
			out = append(out, DefaultModelMeta(id))
		}
	}
	return out, nil
}

// UpsertModelMetas sanitizes and stores one row per entry (insert or
// overwrite). Empty input is a no-op.
func (s *Store) UpsertModelMetas(ctx context.Context, metas []ModelMeta) error {
	if len(metas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := upsertModelMetas(ctx, tx, metas, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// upsertModelMetas is the transaction-scoped core of UpsertModelMetas.
func upsertModelMetas(ctx context.Context, tx *sql.Tx, metas []ModelMeta, now int64) error {
	for _, m := range metas {
		SanitizeMeta(&m)
		if m.ModelID == "" {
			continue
		}
		in, _ := json.Marshal(m.InputModality)
		out, _ := json.Marshal(m.OutputModality)
		efforts, _ := json.Marshal(m.ReasoningEfforts)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO models (model_id, input_modality, output_modality, context_length, reasoning_efforts, reasoning_default, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(model_id) DO UPDATE SET
				input_modality = excluded.input_modality,
				output_modality = excluded.output_modality,
				context_length = excluded.context_length,
				reasoning_efforts = excluded.reasoning_efforts,
				reasoning_default = excluded.reasoning_default,
				updated_at = excluded.updated_at`,
			m.ModelID, string(in), string(out), m.ContextLength, string(efforts), m.ReasoningDefault, now); err != nil {
			return err
		}
	}
	return nil
}

// pruneModelMetas is the transaction-scoped core of metadata pruning
// (used by persist when the whitelist shrinks). An empty keep list clears
// the whole table.
func pruneModelMetas(ctx context.Context, tx *sql.Tx, keep []string) error {
	if len(keep) == 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM models`)
		return err
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM models WHERE model_id NOT IN (`+placeholders(len(keep))+`)`, args(keep)...)
	return err
}

func scanModelMeta(rows *sql.Rows) (ModelMeta, error) {
	var (
		m                ModelMeta
		in, out, efforts string
	)
	if err := rows.Scan(&m.ModelID, &in, &out, &m.ContextLength, &efforts, &m.ReasoningDefault); err != nil {
		return m, err
	}
	// Stored rows are sanitized on write; parse errors fall back to the
	// column defaults rather than failing the whole read.
	m.InputModality = parseModalities(in)
	m.OutputModality = parseModalities(out)
	m.ReasoningEfforts = filterEmpty(parseStrings(efforts))
	if len(m.ReasoningEfforts) == 0 {
		m.ReasoningEfforts = append([]string(nil), DefaultReasoningEfforts...)
	}
	if m.ContextLength <= 0 {
		m.ContextLength = DefaultContextLength
	}
	return m, nil
}

func parseStrings(s string) []string {
	var out []string
	if s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

func parseModalities(s string) []string {
	return filterModalities(parseStrings(s))
}

func placeholders(n int) string {
	return strings.Repeat("?,", n-1) + "?"
}

func args(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
