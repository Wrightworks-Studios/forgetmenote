# Organization automation configuration

Keep workflow packages, prompts, tests, connector bindings and installation
instructions in your organization’s package repository. Connect that repository
as a GitHub-authoritative notebook, then use Administration → Automation to
prepare, install and activate approved entrypoints at pinned revisions.

See the [host contract](../../../docs/automation-packages.md). Repository pulls
never activate changed code. Keep credentials in instance configuration;
packages use checked host capabilities through the server’s Python runtime.

Simple declarative examples are in [automations.example.json](../automations.example.json).
Warehouse validation/repair, profile maintenance, activity aggregation, Drive
import and storage/index health are reusable platform capabilities.
