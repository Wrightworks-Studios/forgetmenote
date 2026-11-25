# Images, files, and video

While editing a note, choose **Insert → Image or file…** in the formatting toolbar, in any mode. Images appear inline in Visual and in the rendered side of Side by side; other files become links. Uploaded MP4 and WebM files become inline videos with playback controls. Uploads are committed immediately; save the note to keep its inserted link. The existing limit is 32 MiB per file. Files stay in the project's Git repository and require project access to download or play.

Choose **Insert → Video embed…** and paste a YouTube or Vimeo video URL. You can also enter a project MP4/WebM path. Visual and reading view show the player; the rendered side of Side by side shows the link. Embedding depends on the provider allowing that video to be embedded; the visible link remains available if playback fails. External players contact the provider when loaded and need an Internet connection. They do not autoplay. Remote video content is not included in repository backups.

A private draft holds its YouTube and Vimeo videos: it never loads their players by itself, because a player would tell the provider your address and the video. Private drafts are text the server does not hold yet: a new note before its first save, a historical version loaded into the editor, and a draft kept on this device after a crash or a refused live update. A draft recovered when a note opens holds them too, until live editing takes it. When live editing cannot take it (the draft has no live record or one of an older live document, the note is `AGENTS.md`, or live editing is unavailable), the draft is on this device only and stays held until you save or reconcile it. In a held draft, Visual shows a placeholder naming the player's host (`www.youtube-nocookie.com` or `player.vimeo.com`) and the link, and only its **Load video** button loads that link's player; another link to the same video keeps its own placeholder. A loaded player stays as long as its link does, until the editor loads the note's text again (opening the note, a historical version or a reconciled draft); a link changed to another address is held again. A player showing in Visual when a saved note becomes private, as when the server refuses a live update, keeps playing. Read's preview of a held draft shows the link only. Saving the draft shows every player at once. Project MP4/WebM files are served by this app and play at once. The placeholder is never part of the note.

The underlying note uses an ordinary Markdown link with the title `video`, in its own paragraph:

```markdown
[Video](https://www.youtube.com/watch?v=M7lc1UVf-VE "video")

[Clip](/assets/clip.mp4 "video")

![Image description](/assets/image.png)
```

Other Markdown viewers can still open the video link. Ordinary links remain links; only standalone links marked with the `video` title render players. Player HTML is never saved in the note. Editing the video link or removing its title updates/removes the player. Its Markdown shows when the caret is on the link's line in Visual, and always in Source.

Supported external URLs include YouTube watch/share/shorts/live/embed URLs and Vimeo numeric video URLs (including unlisted video hashes). Other providers, playlists, arbitrary iframe HTML, externally hosted MP4/WebM files, and externally hosted images are not enabled. Upload images into the project instead. Extra YouTube playback parameters such as timestamps are not carried into the embedded player.

The app permits frames only from `www.youtube-nocookie.com` and `player.vimeo.com`; local videos use the existing authenticated asset route. If your reverse proxy supplies an additional Content-Security-Policy, it must allow those frame origins and same-origin media as well.

Provider references checked 2026-09-07: [YouTube embedding](https://developers.google.com/youtube/player_parameters), [Vimeo player parameters](https://help.vimeo.com/hc/en-us/articles/12426260232977-About-Player-Parameters).
