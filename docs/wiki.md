# Wiki mode

Wiki mode reads a shared notes notebook as a site. Pages stay ordinary Markdown
notes with their Git history; wiki mode only arranges and links them.

## Opening a notebook as a wiki

A notebook Owner turns on **Notebook settings → Sharing → Open as wiki**. Members
then open the notebook in the wiki layout by default. Anyone can use **Editor
layout** in the sidebar to switch back, and **Read as wiki** to return; each
browser remembers that choice per notebook. A link with `layout=wiki` or
`layout=editor` opens that layout. Repository notebooks and the private Inbox
have no wiki layout.

The setting is stored with the notebook's membership in the instance catalog, not
in notebook content. MCP clients change it with `set_notebook_wiki` (Owner/Admin)
and read it as `openAsWiki` from `list_projects`.

## Reading

- **Home page.** The notebook's `index.md` is the front page. Without one, the
  wiki shows a generated contents list. A folder opens on its own `index.md`, or
  lists its pages.
- **Page tree.** The sidebar lists folders and pages. An optional numeric `order`
  front-matter value sorts pages first, ascending; the rest follow by title. A
  folder takes its title and order from its `index.md`. `AGENTS.md` and Trash are
  not pages.

  ```markdown
  ---
  title: Getting started
  order: 1
  ---
  ```

  A value that is not one plain number is ignored and stays in the Markdown.
- **Previous and next.** Each page ends with links to the previous and next page
  in its folder, starting from the folder's overview.
- **Referenced by.** The footer lists pages that link to this one, from the same
  evidence as the note context panel, including permitted pages in other notebooks.
- **Search.** The sidebar search box searches this notebook with hybrid search.
- **Editing.** Editors use **Edit page**, which opens the ordinary editor layout.

## Page links

Ordinary Markdown links keep working. In either layout, `[[Page title]]` links to
the page with that title (any case; the page nearest the linking page wins), and
otherwise to a path from the notebook root, then from the linking page's folder,
such as `[[guide/start]]`. `[[Target|label]]` shows a different label. Text inside
code spans and code blocks is left as written.

A link to a page that does not exist yet is dashed. Editors can select it to open
**New note** with the name filled in. Only notes in the notes collection are pages:
a draft or archived note with the title does not count.

The server resolves page links the same way, so a page linked only by `[[…]]`
lists the linking page under **Referenced by**, in the note context panel and in
note maps. A link names a title, not a file: when a page is retitled, links to its
old title follow whichever page now has it, or become dashed. Moving or renaming a
page does not rewrite `[[path]]` links.
