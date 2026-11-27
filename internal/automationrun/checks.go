package automationrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/connectors"
)

type recipeFailure struct{ error }

// RecipeBlocks returns a recipe's named SQL examples by name.
func RecipeBlocks(content string) (map[string]string, error) {
	blocks := map[string]string{}
	name := ""
	var body strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if name == "" {
			if strings.HasPrefix(line, "```sql ") {
				name = strings.TrimSpace(strings.TrimPrefix(line, "```sql "))
				if name == "" || blocks[name] != "" {
					return nil, errors.New("ambiguous SQL example")
				}
				body.Reset()
			}
			continue
		}
		if line == "```" {
			blocks[name] = body.String()
			name = ""
		} else {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	if name != "" {
		return nil, errors.New("unclosed SQL example")
	}
	return blocks, nil
}

// CheckRecipe runs a run's checks against the SQL examples in a recipe's
// content; a failure a model may repair is a recipeFailure.
func (s *Service) CheckRecipe(ctx context.Context, r *automation.Run, content string) error {
	blocks, e := RecipeBlocks(content)
	if e != nil {
		return recipeFailure{e}
	}
	for _, check := range r.Spec.Checks {
		sql, ok := blocks[check.Block]
		if !ok {
			return recipeFailure{fmt.Errorf("missing SQL example %s", check.Block)}
		}
		call := check.Call
		call.Arguments = map[string]any{}
		for k, v := range check.Call.Arguments {
			call.Arguments[k] = v
		}
		call.Arguments["sql"] = sql
		result, e := s.Call(ctx, r, call)
		if e != nil {
			if errors.Is(e, connectors.ErrQuery) {
				return recipeFailure{fmt.Errorf("check %s: %w", check.Name, e)}
			}
			return fmt.Errorf("check %s: %w", check.Name, e)
		}
		if !result.Receipt.Complete {
			return fmt.Errorf("check %s incomplete", check.Name)
		}
		if err := checkAutomationAssertions(check, result.Data, result.Receipt.DurationMS); err != nil {
			return err
		}
		if check.Reference != nil {
			reference, err := s.Call(ctx, r, *check.Reference)
			if err != nil {
				return err
			}
			if !reference.Receipt.Complete || reference.Receipt.OutputHash != result.Receipt.OutputHash {
				return recipeFailure{fmt.Errorf("check %s differs from independent reference", check.Name)}
			}
		}
	}
	return nil
}

func (s *Service) runAutomationChecks(ctx context.Context, r *automation.Run, save func() error) error {
	checks, err := s.EvaluateChecks(ctx, r, nil)
	r.CheckReport = &automation.CheckReport{State: "healthy", Checks: checks}
	if err != nil {
		r.CheckReport.State = "failed"
		r.Result = CheckReportText(r.CheckReport) + "\n" + ReceiptSummary(r.Receipts)
		if e := save(); e != nil {
			return e
		}
		return err
	}
	return s.automationReport(ctx, r, CheckReportText(r.CheckReport)+"\n"+ReceiptSummary(r.Receipts), save)
}

func checkAutomationAssertions(check automation.Check, data any, duration int64) error {
	for _, a := range check.Assertions {
		v, ok := automation.Pointer(data, a.Pointer)
		var expected any
		if json.Unmarshal(a.Equals, &expected) != nil || !ok || automation.Hash(v) != automation.Hash(expected) {
			return recipeFailure{fmt.Errorf("check %s failed at %s", check.Name, a.Pointer)}
		}
	}
	if check.MaxDurationMS > 0 && duration > check.MaxDurationMS {
		return recipeFailure{fmt.Errorf("check %s exceeded %d ms", check.Name, check.MaxDurationMS)}
	}
	return nil
}

// CheckExcerpt is a check result's bounded diagnostic excerpt.
func CheckExcerpt(data any) json.RawMessage {
	raw, _ := json.Marshal(data)
	if len(raw) > 4096 {
		raw, _ = json.Marshal(map[string]any{"excerpt": string(raw[:4000]), "truncated": true})
	}
	return raw
}

// Every check runs, including fixed guards. Full results are compared locally;
// bounded excerpts are diagnostic evidence, never the comparison input.
func (s *Service) EvaluateChecks(ctx context.Context, r *automation.Run, blocks map[string]string) ([]automation.CheckResult, error) {
	out := make([]automation.CheckResult, 0, len(r.Spec.Checks))
	var failures []error
	for _, c := range r.Spec.Checks {
		v := automation.CheckResult{Name: c.Name, Block: c.Block, State: "passed"}
		call := c.Call
		var err error
		if c.Block != "" {
			var sql string
			sql, err = connectors.ReadQueryFragment(blocks[c.Block])
			if err == nil {
				if c.SQLTemplate != "" {
					sql = strings.Replace(c.SQLTemplate, "{{sql}}", "\n"+sql+"\n", 1)
				}
				call.Arguments = map[string]any{}
				for k, v := range c.Call.Arguments {
					call.Arguments[k] = v
				}
				call.Arguments["sql"] = sql
			}
		}
		var result, reference connectors.Result
		if err == nil {
			result, err = s.Call(ctx, r, call)
		}
		if err == nil && !result.Receipt.Complete {
			err = errors.New("incomplete check result")
		}
		v.Actual = CheckExcerpt(result.Data)
		if err == nil {
			err = checkAutomationAssertions(c, result.Data, result.Receipt.DurationMS)
		}
		if err == nil && c.Reference != nil {
			reference, err = s.Call(ctx, r, *c.Reference)
			v.Reference = CheckExcerpt(reference.Data)
			if err == nil && !reference.Receipt.Complete {
				err = errors.New("incomplete independent reference")
			}
			if err == nil && result.Receipt.OutputHash != reference.Receipt.OutputHash {
				err = recipeFailure{errors.New("differs from independent reference")}
			}
		}
		if err != nil {
			v.State = "unavailable"
			var failed recipeFailure
			if errors.As(err, &failed) || errors.Is(err, connectors.ErrQuery) {
				v.State = "failed"
			}
			v.Error = chatrun.TextLimit(err.Error(), 1000)
			failures = append(failures, fmt.Errorf("check %s: %w", c.Name, err))
		}
		v.Fingerprint = automation.Hash([]any{result.Receipt.SchemaHash, result.Receipt.OutputHash, reference.Receipt.SchemaHash, reference.Receipt.OutputHash, v.State, v.Error})
		out = append(out, v)
	}
	return out, errors.Join(failures...)
}

// CheckReportText is a check report as Markdown.
func CheckReportText(report *automation.CheckReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", report.State, report.Summary)
	for _, c := range report.Checks {
		fmt.Fprintf(&b, "- %s: %s", c.Name, c.State)
		if c.Error != "" {
			fmt.Fprintf(&b, " (%s)", c.Error)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
