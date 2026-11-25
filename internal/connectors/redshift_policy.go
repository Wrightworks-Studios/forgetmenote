package connectors

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// IAM credentials use the SDK's refreshable runtime chain (including pod roles).
// SecretARN refers to an AWS-managed database credential, never its value.
type RedshiftConfig struct {
	Region        string   `json:"region"`
	Database      string   `json:"database"`
	Cluster       string   `json:"cluster,omitempty"`
	Workgroup     string   `json:"workgroup,omitempty"`
	DBUser        string   `json:"dbUser,omitempty"`
	SecretARN     string   `json:"secretArn,omitempty"`
	Schemas       []string `json:"schemas,omitempty"`
	DeniedColumns []string `json:"deniedColumns,omitempty"`
	MaxCells      int      `json:"maxCells,omitempty"`
}

type sqlToken struct {
	text               string
	identifier, quoted bool
}

func (t sqlToken) keyword(s string) bool { return !t.quoted && t.text == s }

// This is a conservative expression guard, not a SQL execution engine. The
// database identity remains authoritative for views, functions and permissions.
func readExpression(sql string) (string, []sqlToken, error) {
	sql = strings.TrimSuffix(strings.TrimSpace(sql), ";")
	tokens := []sqlToken{}
	for i := 0; i < len(sql); {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			if n := strings.IndexByte(sql[i:], '\n'); n >= 0 {
				i += n + 1
			} else {
				i = len(sql)
			}
		case strings.HasPrefix(sql[i:], "/*"):
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				if strings.HasPrefix(sql[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(sql[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return "", nil, ErrQuery
			}
		case sql[i] == '\'' || sql[i] == '"':
			quote := sql[i]
			i++
			var value strings.Builder
			closed := false
			for i < len(sql) {
				if sql[i] == quote {
					i++
					if i < len(sql) && sql[i] == quote {
						value.WriteByte(quote)
						i++
						continue
					}
					closed = true
					break
				}
				if sql[i] == '\\' {
					return "", nil, ErrQuery
				}
				value.WriteByte(sql[i])
				i++
			}
			if !closed {
				return "", nil, ErrQuery
			}
			if quote == '"' {
				tokens = append(tokens, sqlToken{strings.ToLower(value.String()), true, true})
			} else {
				tokens = append(tokens, sqlToken{"literal", false, true})
			}
		case sql[i] == ';' || sql[i] == '$' || sql[i] >= 128:
			return "", nil, ErrQuery
		case sqlLetter(sql[i]):
			start := i
			i++
			for i < len(sql) && (sqlLetter(sql[i]) || sql[i] >= '0' && sql[i] <= '9') {
				i++
			}
			word := strings.ToLower(sql[start:i])
			if strings.Contains(" insert update delete merge create alter drop copy unload call grant revoke truncate vacuum analyze into set reset begin commit rollback ", " "+word+" ") {
				return "", nil, ErrQuery
			}
			tokens = append(tokens, sqlToken{word, true, false})
		default:
			if !strings.ContainsRune(" \t\r\n", rune(sql[i])) {
				tokens = append(tokens, sqlToken{text: string(sql[i])})
			}
			i++
		}
	}
	if len(tokens) == 0 || !tokens[0].keyword("select") && !tokens[0].keyword("with") {
		return "", nil, ErrQuery
	}
	return sql, tokens, nil
}

func sqlLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_' }

// ReadQueryFragment checks an independently generated query before embedding it
// in an operator-owned assertion. Balanced parentheses keep it inside the CTE;
// the connector still enforces its own source/column and database permissions.
func ReadQueryFragment(sql string) (string, error) {
	query, tokens, err := readExpression(sql)
	if err != nil {
		return "", err
	}
	depth, source := 0, false
	for _, t := range tokens {
		if t.quoted {
			continue
		}
		switch t.text {
		case "(":
			depth++
		case ")":
			depth--
		case "from":
			source = true
		}
		if depth < 0 {
			return "", ErrQuery
		}
	}
	if depth != 0 || !source {
		return "", ErrQuery
	}
	return query, nil
}

func (c *RedshiftConfig) validatePolicy() error {
	for _, list := range [][]string{c.Schemas, c.DeniedColumns} {
		for _, name := range list {
			if name != strings.ToLower(name) || !identifier.MatchString(name) {
				return fmt.Errorf("Redshift policy identifiers must be lowercase names")
			}
		}
	}
	if c.MaxCells < 0 || c.MaxCells > 1000000 {
		return fmt.Errorf("Redshift maxCells must be between 1 and 1000000, or zero for the row/byte limits only")
	}
	return nil
}

func (c *RedshiftConfig) checkQuery(tokens []sqlToken, inherited map[string]bool) error {
	if len(c.Schemas) == 0 && len(c.DeniedColumns) == 0 {
		return nil
	}
	ctes := maps.Clone(inherited)
	if ctes == nil {
		ctes = map[string]bool{}
	}
	for i, t := range tokens {
		if t.keyword("table") {
			return fmt.Errorf("%w: use SELECT with explicit columns and schema-qualified tables", ErrQuery)
		}
		if t.identifier && slices.Contains(c.DeniedColumns, t.text) {
			return fmt.Errorf("%w: column excluded by the connection policy", ErrQuery)
		}
		if t.text == "*" && len(c.DeniedColumns) > 0 {
			// COUNT(*) and multiplication do not project every source column.
			count := i >= 2 && tokens[i-1].text == "(" && tokens[i-2].keyword("count") && i+1 < len(tokens) && tokens[i+1].text == ")"
			multiply := i > 0 && i+1 < len(tokens) && sqlOperand(tokens[i-1]) && sqlOperand(tokens[i+1])
			if !multiply && !count {
				return fmt.Errorf("%w: select explicit columns", ErrQuery)
			}
		}
	}
	// CTE visibility is scoped to this expression; names in nested expressions
	// cannot authorize unqualified tables in the parent or a sibling expression.
	i := 0
	if len(tokens) > 0 && tokens[0].keyword("with") {
		i++
		recursive := i < len(tokens) && tokens[i].keyword("recursive")
		if recursive {
			i++
		}
		for {
			if i >= len(tokens) || !tokens[i].identifier {
				return ErrQuery
			}
			name := tokens[i].text
			i++
			if i < len(tokens) && tokens[i].text == "(" {
				end := sqlParenEnd(tokens, i)
				if end < 0 {
					return ErrQuery
				}
				i = end + 1
			}
			if i+1 >= len(tokens) || !tokens[i].keyword("as") || tokens[i+1].text != "(" {
				return ErrQuery
			}
			i++
			end := sqlParenEnd(tokens, i)
			if end < 0 {
				return ErrQuery
			}
			if recursive {
				ctes[name] = true
			}
			if err := c.checkQuery(tokens[i+1:end], ctes); err != nil {
				return err
			}
			ctes[name] = true
			i = end + 1
			if i < len(tokens) && tokens[i].text == "," {
				i++
				continue
			}
			break
		}
	}
	from := false
	for ; i < len(tokens); i++ {
		t := tokens[i]
		if t.keyword("where") || t.keyword("group") || t.keyword("having") || t.keyword("order") || t.keyword("limit") || t.keyword("union") || t.keyword("except") || t.keyword("intersect") {
			from = false
		}
		if t.text == "," && from {
			return fmt.Errorf("%w: use explicit JOIN instead of comma joins", ErrQuery)
		}
		if t.keyword("from") || t.keyword("join") {
			from = true
			j := i + 1
			if j >= len(tokens) {
				return ErrQuery
			}
			if tokens[j].text == "(" {
				if j+1 >= len(tokens) || !tokens[j+1].keyword("select") && !tokens[j+1].keyword("with") {
					return ErrQuery
				}
			} else {
				if !tokens[j].identifier {
					return ErrQuery
				}
				name := tokens[j].text
				j++
				if j < len(tokens) && tokens[j].text == "." {
					if j+1 >= len(tokens) || !tokens[j+1].identifier || len(c.Schemas) > 0 && !slices.Contains(c.Schemas, name) {
						return fmt.Errorf("%w: schema excluded by connection policy", ErrQuery)
					}
					j += 2
				} else if !ctes[name] {
					return fmt.Errorf("%w: use an allowed schema-qualified table", ErrQuery)
				}
				if j < len(tokens) && (tokens[j].text == "." || tokens[j].text == "(") {
					return ErrQuery
				}
				i = j - 1
			}
		}
		if tokens[i].text == "(" {
			end := sqlParenEnd(tokens, i)
			if end < 0 {
				return ErrQuery
			}
			inner := tokens[i+1 : end]
			if slices.ContainsFunc(inner, func(t sqlToken) bool { return t.keyword("select") || t.keyword("with") }) {
				if err := c.checkQuery(inner, ctes); err != nil {
					return err
				}
			}
			i = end
		}
	}
	return nil
}

func sqlOperand(t sqlToken) bool {
	if t.identifier && !t.quoted && strings.Contains(" select distinct all from where group order having limit join on as union except intersect ", " "+t.text+" ") {
		return false
	}
	return t.identifier || t.quoted || t.text == "(" || t.text == ")" || len(t.text) == 1 && t.text[0] >= '0' && t.text[0] <= '9'
}

func sqlParenEnd(tokens []sqlToken, start int) int {
	depth := 0
	for i := start; i < len(tokens); i++ {
		if tokens[i].text == "(" {
			depth++
		}
		if tokens[i].text == ")" {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
