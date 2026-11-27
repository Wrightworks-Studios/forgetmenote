# Notebooks and people

A notebook is the existing project: one Git repository, one membership list, and independent content, locks, sources, search, and history. One organization occupies each instance. Flat groups can grant inherited notebook access; see [organization administration](instance-administration.md). Existing project IDs, URLs, MCP tool names, and `/data/projects/` paths stay valid. Existing memberships, account identities, Git authorship, and GitHub links are preserved.

## Instance administration

Admins open **Administration → People** to search accounts, choose a person, and see their notebook access. They can set a display name, grant or revoke instance Admin authority, allow notebook creation, disable an account, revoke its browser and AI sessions or any one of its API credentials, or link an identity provider's sign-in identity to it. Display names are labels only; stable internal IDs still determine identity and access. Duplicate names remain distinguishable by their IDs.

**Can create notebooks** is a separate, default-off account permission. Admins always have this ability. Giving someone ownership of an existing notebook does not silently grant creation access; an Admin can explicitly enable it. Revoking creation access leaves existing memberships intact. Disabled users cannot create or access notebooks, even if a creation grant remains stored.

Admins can create copyable invitation links. With local login enabled, people choose their own passwords, and Admins can issue password-reset links. Existing direct account-provisioning API operations remain supported. With OIDC or trusted-proxy authentication, credentials remain with the identity provider. OIDC accounts appear after their first verified login with no creation permission or Admin authority. Notebook access remains empty unless the operator explicitly configures enrollment in existing groups through `OIDC_DEFAULT_GROUPS`. Admins can assign readable display names after provisioning. Adding a notebook member does not provision provider credentials.

## Employee work notebooks

Administration → People → select a person → **Provision a work notebook** creates
an ordinary notes notebook with that person as its sole Owner. Choose an explicit
name and ID. An exact repeat reuses that same private notebook; it refuses a
conflicting, shared, repository or protected Inbox notebook. For another existing
notebook, inspect its members and explicitly grant ownership through Notebook
settings → People. No special personal storage type or agent management consent
is needed.

Ownership of this notebook grants neither instance administration nor notebook
creation. Other employees have no access unless shared; instance administrators
retain authority. Group grants and direct grants combine by highest role.
Repository authority, note locks and revision checks remain independent.
Protected Inbox privacy/consent stays separate.

Employees open **Account actions → Connect your AI** to install their companion
and inspect effective destinations. Their agent should discover writable notebooks
and ask for an explicit destination when more than one could match, never guess
from an email or notebook name. See [AI setup](ai.md).

## Importing a notebook

Choose **Import notebook…** from the top dropdown, or switch to **Import notebook** inside the creation dialog. Upload a complete bundle, enter a new name and ID, and import. The importer becomes Owner; existing notebooks and memberships stay unchanged. See [bundle limits and restoration](backups.md#notebook-export-and-import). Internal project IDs, API fields, tool names, and storage paths remain compatible with earlier versions.

## Owner-managed notebooks

Authorized creators open the top notebook dropdown → **Create notebook…**, enter a name, and review the automatically suggested ID. The creator becomes Owner. Other users receive no membership; instance Admins retain access to all notebooks.

An Owner opens **Notebook settings → People** to:

- Choose a person from the searchable dropdown and add them as Reader, Commenter, Editor, or Owner.
- Change or remove a member's role.
- Assign another active Owner before removing or demoting the last Owner.

Search requires at least two characters and returns at most 50 matching names and stable IDs, without unrelated memberships, provider identities, or account credentials. An exact internal ID also finds a person whose label is not known. The member list shows effective Admin or disabled status. Owner administration returns only owned notebooks and their members, never the complete instance user directory.

Owners cannot disable accounts, revoke someone else's sessions, reset passwords, grant Admin authority or notebook-creation access, or manage a notebook they do not own. Editors cannot manage membership or locks.

Disabling an account remains an Admin security action even when that user is the sole Owner. Its memberships stay recorded, and Admins can add a replacement Owner. Legacy notebooks without an explicit Owner remain accessible to Admins; they are not silently reassigned during an upgrade.

## AI and API behavior

Browser and MCP operations use the same catalog checks. MCP management also requires the user's explicit `notes:manage` OAuth consent:

- `capabilities` includes `canCreateProjects`, taking consent into account.
- `create_project` creates a private notebook and makes the authenticated caller Owner.
- `provision_work_notebook` lets an Admin explicitly provision an ordinary work notebook for an enabled person. It requires `notes:manage` on MCP, not on the employee’s connection.
- `administration` returns all accounts/notebooks to Admins; Owners see their notebooks and members. Authorized creators with no owned notebooks receive an empty list.
- `find_workspace_people` performs the scoped member search using `project` and `query`.
- `set_project_member` enforces notebook authority and last-Owner protection.
- `set_user_access` is Admin-only and accepts optional `displayName`, `canCreateProjects`, `admin`, and `disabled` fields. Omitted fields are preserved, including when older clients send only the original flags.

HTTP equivalents retain `/api/projects`, `/api/admin`, `/api/users`, and `/api/members`; member search uses `GET /api/people?project=ID&q=QUERY`. Instance permissions and display names live in the existing private catalog, outside content Git history, and are included in full-instance backups. A Git remote does not back up accounts or permissions.

## Verification

Focused catalog and raw HTTP/MCP tests cover default-denied creation, creation grants and ownership across restart, disabled creators, private notebook boundaries, scoped search, duplicate names, last-Owner conflicts, Admin-only account changes, and compatibility of partial updates. The browser suite uses disposable Admin/Owner sessions to grant creation access, create a notebook, find and add a Reader, refuse last-Owner removal and Admin escalation, and verify session denial after disablement. Desktop and mobile screenshots were inspected in Light and Dark themes.

The full Go race suite, vet, 35 frontend tests, and all four Chromium journeys passed. The Docker smoke restores a complete disposable instance and verifies creation grants, display names, membership, and content. The Authelia OIDC/MCP consent and full-restoration smoke also passed. External provider registration, real user provisioning, and a remote hosting rollout were not performed for this change.

Notebook switching and creation share the top dropdown; Owners have no separate Notebooks sidebar entry. Administration remains Admin-only. The people picker supports typing, arrow-key selection, Enter, Escape, and outside-click dismissal. Note and folder protection lives in their three-dot menus and the note header rather than Notebook settings.

The updated browser checks also cover creation from the top selector with no Owner sidebar shortcut, keyboard selection in the people dropdown, viewport-constrained mobile positioning, and folder-lock inheritance/unlocking through note and folder controls.
