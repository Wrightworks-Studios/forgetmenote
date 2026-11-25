package actions

// Notebook aliases change discovery language, not argument schemas or policy.
// Persisted operations and current clients retain their existing action names.
var notebookNames = map[string]string{
	"list_projects":            "list_notebooks",
	"project_status":           "notebook_status",
	"create_project":           "create_notebook",
	"import_project_bundle":    "import_notebook_bundle",
	"export_project_backup":    "export_notebook_backup",
	"set_project_member":       "set_notebook_member",
	"project_groups":           "notebook_groups",
	"set_project_group":        "set_notebook_group",
	"project_instructions":     "notebook_instructions",
	"set_project_instructions": "set_notebook_instructions",
	"read_project_changes":     "read_notebook_changes",
}

func NotebookAlias(name string) string { return notebookNames[name] }
func LegacyName(name string) string {
	for legacy, notebook := range notebookNames {
		if notebook == name {
			return legacy
		}
	}
	return name
}
