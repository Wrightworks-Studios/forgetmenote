package actions

import (
	"encoding/json"
	"forgetmenote/internal/workspace"
	"maps"
	"slices"
)

// Action scope describes argument binding, never permission to read a notebook.
// Handlers still check current membership, consent and the frozen chat corpus.
type Scope uint8

const (
	CurrentNotebook Scope = iota
	SelectedRepository
	SelectedRepositories
	SelectedDocument
	SelectedNotebooks
)

type Policy struct {
	Chat, Read, Proposal, Edit, Control, Replay bool
	Scope                                       Scope
	SourceContent                               bool
}

// Static chat availability, consent and replay classification have one owner.
// An explicit entry without Chat excludes an MCP action; new/unclassified actions
// remain unavailable until their corpus and consent boundaries have been reviewed.
var policies = map[string]Policy{
	"prepare_source": {}, "source_job": {}, "cancel_source_job": {}, "resume_source_job": {}, "import_source_job": {},
	"recover_assistant_work": {}, "assistant_activity": {}, "dismiss_assistant_draft": {}, "prepare_assistant_draft": {}, "assistant_directory": {}, "set_assistant_responsibilities": {},
	"assistant_policy": {}, "set_assistant_policy": {},
	"read_personal_source":   {Chat: true, Read: true, Scope: SelectedNotebooks},
	"submit_personal_action": {},
	"assistant_status":       {Chat: true, Read: true, Scope: SelectedNotebooks}, "save_assistant_settings": {},
	"assistant_context": {Chat: true, Read: true, Scope: SelectedNotebooks}, "update_assistant_memory": {},
	// Agent memory is not a chat tool. Direct messaging conversations recall and remember through
	// the server (messaginghttp/memory.go), which marks the run as it does for assistant_context.
	"recall_memory": {}, "remember": {}, "confirm_memory": {},
	// Task queues are personal and written only by task actions; chat reaches
	// them through deterministic Slack commands and buttons, not tools.
	"list_tasks": {}, "add_task": {}, "update_task": {}, "task_history": {}, "task_queues": {}, "set_task_queues": {},
	"list_data_connections":        {Chat: true, Read: true},
	"list_data_operations":         {Chat: true, Read: true},
	"call_data_operation":          {Chat: true, Read: true},
	"data_connection_access":       {},
	"set_data_connection_access":   {},
	"data_connection_effects":      {},
	"automation_effects":           {},
	"resolve_automation_effect":    {},
	"bind_external_identity":       {},
	"organization_status":          {},
	"provision_work_notebook":      {},
	"ai_settings":                  {},
	"set_ai_settings":              {},
	"chat_clients":                 {},
	"chat_client_directory":        {},
	"set_chat_clients":             {},
	"set_instance_name":            {},
	"set_instance_branding":        {},
	"discover_automation_packages": {}, "install_automation_package": {}, "prepare_automation_package": {}, "save_automation": {}, "delete_automation": {}, "validate_automation": {}, "test_automation": {}, "automation_run": {}, "automation_status": {}, "run_automation": {}, "cancel_automation": {},
	"find_recipes":                {Chat: true, Read: true, Replay: true, Scope: SelectedDocument},
	"set_group":                   {},
	"delete_group":                {},
	"project_groups":              {},
	"set_project_group":           {},
	"create_invitation":           {},
	"revoke_invitation":           {},
	"create_password_reset":       {},
	"create_service_account":      {},
	"set_service_steward":         {},
	"issue_service_credential":    {},
	"revoke_user_credential":      {},
	"offboarding_status":          {},
	"reassign_notebook_ownership": {},

	// Agents only prepare a purge; a person confirms it in the browser.
	"prepare_purge": {},

	"preview_notebook_authority_change": {},
	"apply_notebook_authority_change":   {},
	"cancel_notebook_authority_change":  {},
	"begin_github_connection":           {},
	"github_connection_status":          {},
	"finish_github_connection":          {},
	"notebook_authority_status":         {},
	"create_api_credential":             {},
	"list_api_credentials":              {},
	"revoke_api_credential":             {},
	"search_notebooks":                  {Chat: true, Read: true, Replay: true, Scope: SelectedNotebooks},
	"read_document":                     {Chat: true, Read: true, Replay: true, Scope: SelectedDocument},
	"list_documents":                    {Chat: true, Read: true, Replay: true, Scope: SelectedDocument},
	"notebook_map":                      {Chat: true, Read: true, Replay: true, Scope: SelectedDocument},
	"document_outline":                  {Read: true, Replay: true, Scope: SelectedDocument},
	"find_term_mentions":                {Read: true, Replay: true, Scope: SelectedDocument},
	"resolve_notebook_link":             {Read: true},
	"find_notebook_link_targets":        {Read: true},
	"retain_repository_citation":        {},
	"search_repository_sources":         {Chat: true, Read: true, Replay: true, Scope: SelectedRepositories},
	"repository_source_status":          {Chat: true, Read: true, Replay: true, Scope: SelectedRepository},
	"repository_source_tree":            {Chat: true, Read: true, Replay: true, Scope: SelectedRepository},
	"repository_source_map":             {Chat: true, Read: true, Replay: true, Scope: SelectedRepository},
	"repository_source_file":            {Chat: true, Read: true, Replay: true, Scope: SelectedRepository, SourceContent: true},
	"repository_source_changes":         {Read: true},
	"configure_repository_index":        {},
	"refresh_repository_source":         {},
	"managed_backup_status":             {Read: true},
	"control_managed_backup":            {},
	"managed_backup_overview":           {Read: true},
	"add_comment":                       {Chat: true, Edit: true},
	"administration":                    {}, // Deliberately excluded from embedded chat.
	"archive_source":                    {Chat: true, Control: true},
	"browse_folder":                     {Chat: true, Read: true, Replay: true},
	"capabilities":                      {}, // Deliberately excluded from embedded chat.
	"checkpoint_live_note":              {Chat: true, Edit: true},
	"complete_workflow":                 {Chat: true, Edit: true, Replay: true},
	"configure_semantic_search":         {Chat: true, Control: true},
	"consumer_checkpoint":               {Chat: true, Read: true, Replay: true},
	"create_project":                    {}, // Deliberately excluded from embedded chat.
	"import_repository":                 {}, // Explicit instance-admin provisioning only.
	"delete_comment":                    {Chat: true, Control: true},
	"duplicate_notes":                   {Chat: true, Control: true},
	"empty_trash":                       {Chat: true, Control: true},
	"export_project_backup":             {Read: true, Replay: true}, // Deliberately excluded from embedded chat.
	"file_review_finding":               {Chat: true, Proposal: true, Edit: true, Replay: true},
	"find_workspace_people":             {Chat: true, Control: true},
	"get_indexing_passages":             {Read: true, Replay: true}, // Deliberately excluded from embedded chat.
	"get_note_sources":                  {Chat: true, Read: true, Replay: true},
	"get_retrieval_settings":            {Chat: true, Read: true, Replay: true},
	"hybrid_search":                     {Chat: true, Read: true, Replay: true},
	"import_project_bundle":             {}, // Deliberately excluded from embedded chat.
	"knowledge_status":                  {Chat: true, Read: true, Replay: true},
	"list_attachments":                  {Read: true, Replay: true}, // Deliberately excluded from embedded chat.
	"list_comments":                     {Chat: true, Read: true, Replay: true},
	"list_projects":                     {},                         // Deliberately excluded from embedded chat.
	"list_trash":                        {Read: true, Replay: true}, // Deliberately excluded from embedded chat.
	"move_notes":                        {Chat: true, Control: true},
	"note_collections":                  {}, // Deliberately excluded from embedded chat.
	"note_history":                      {Chat: true, Read: true, Replay: true},
	"note_locks":                        {Chat: true, Read: true, Replay: true},
	"patch_live_note":                   {Chat: true, Edit: true, Replay: true},
	"preview_markdown":                  {Chat: true, Read: true, Replay: true},
	"project_instructions":              {Chat: true, Read: true, Replay: true},
	"project_status":                    {Chat: true, Read: true, Replay: true},
	"read_attachment":                   {Read: true, Replay: true}, // Deliberately excluded from embedded chat.
	"read_live_note":                    {Chat: true, Read: true, Replay: true},
	"read_note":                         {Chat: true, Read: true, Replay: true},
	"read_project_changes":              {Chat: true, Read: true, Replay: true},
	"list_sources_awaiting_text":        {Read: true, Replay: true}, // Excluded from embedded chat, which cannot read originals.
	"read_source_original":              {Read: true, Replay: true}, // Deliberately excluded from embedded chat.
	"resolve_comment":                   {Chat: true, Edit: true},
	"restore_note":                      {Chat: true, Control: true},
	"restore_trash":                     {Chat: true, Control: true},
	"review_decision":                   {Chat: true, Control: true},
	"review_inbox":                      {Chat: true, Read: true, Replay: true},
	"save_note":                         {Chat: true, Edit: true},
	"search_notes":                      {Chat: true, Read: true, Replay: true},
	"semantic_search":                   {Chat: true, Read: true, Replay: true},
	"set_note_container":                {Chat: true, Control: true},
	"set_note_lock":                     {Chat: true, Control: true},
	"set_note_properties":               {Chat: true, Edit: true},
	"set_notebook_wiki":                 {Chat: true, Control: true},
	"set_project_instructions":          {Chat: true, Control: true},
	"set_project_member":                {Chat: true, Control: true},
	"set_retrieval_settings":            {Chat: true, Control: true},
	"set_semantic_config":               {Chat: true, Control: true},
	"set_user_access":                   {}, // Deliberately excluded from embedded chat.
	"source_formats":                    {Chat: true, Read: true, Replay: true},
	"submit_change_group":               {Chat: true, Proposal: true, Edit: true, Replay: true},
	"submit_embeddings":                 {Chat: true, Control: true},
	"trash_notes":                       {Chat: true, Control: true},
	"upload_attachment":                 {Chat: true, Edit: true},
	"verify_source":                     {Read: true, Replay: true}, // Deliberately excluded from embedded chat.
}

func LookupPolicy(name string) Policy { return policies[LegacyName(name)] }

func (p Policy) ChatAvailable() bool {
	return p.Chat && (p.Read || p.Proposal || p.Edit || p.Control)
}

func Allowed(mode, name string, args json.RawMessage, enabled []string, approved bool) bool {
	name = LegacyName(name)
	p := LookupPolicy(name)
	if !p.ChatAvailable() {
		return false
	}
	if p.Read {
		return name != "read_live_note" || slices.Contains(enabled, "live")
	}
	if p.Control {
		return approved
	}
	if mode == "ask" {
		return false
	}
	if name == "submit_change_group" {
		var in struct {
			Mode  string              `json:"mode"`
			Group workspace.EditGroup `json:"group"`
		}
		if json.Unmarshal(args, &in) != nil {
			return false
		}
		if in.Mode == "proposal" {
			return true
		}
		for _, e := range in.Group.Edits {
			if e.Delete && !approved {
				return false
			}
		}
		return mode == "edit" && slices.Contains(enabled, "notes")
	}
	if p.Proposal {
		return true
	}
	if mode != "edit" {
		return false
	}
	if name == "patch_live_note" || name == "checkpoint_live_note" {
		return slices.Contains(enabled, "live")
	}
	if name == "complete_workflow" {
		return slices.Contains(enabled, "review")
	}
	if name == "add_comment" || name == "resolve_comment" {
		return slices.Contains(enabled, "comments")
	}
	return p.Edit && slices.Contains(enabled, "notes")
}

func Policies() map[string]Policy {
	out := maps.Clone(policies)
	for legacy, notebook := range notebookNames {
		out[notebook] = out[legacy]
	}
	return out
}
