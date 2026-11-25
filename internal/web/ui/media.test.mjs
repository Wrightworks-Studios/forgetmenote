import test from 'node:test';
import assert from 'node:assert/strict';
import {videoSource, attachmentMarkdown, videoMarkdown, videoHost} from './media.mjs';

test('video links resolve only to supported players or project video files',()=>{
  const youtube={kind:'iframe',url:'https://www.youtube-nocookie.com/embed/M7lc1UVf-VE'};
  for(const url of ['https://www.youtube.com/watch?v=M7lc1UVf-VE','https://youtu.be/M7lc1UVf-VE','https://youtube.com/shorts/M7lc1UVf-VE'])assert.deepEqual(videoSource(url),youtube);
  assert.deepEqual(videoSource('https://vimeo.com/123456789'),{kind:'iframe',url:'https://player.vimeo.com/video/123456789'});
  assert.deepEqual(videoSource('https://vimeo.com/123456789/abcdef0123'),{kind:'iframe',url:'https://player.vimeo.com/video/123456789?h=abcdef0123'});
  assert.deepEqual(videoSource('/assets/clip.mp4'),{kind:'video',url:'/assets/clip.mp4'});
  assert.deepEqual(videoSource('../assets/clip.webm'),{kind:'video',url:'../assets/clip.webm'});
});
test('unsupported and deceptive video URLs never become players',()=>{
  for(const url of ['javascript:alert(1)','data:video/mp4,abc','//evil.test/a.mp4','https://youtube.com.evil.test/watch?v=M7lc1UVf-VE','https://user@youtube.com/watch?v=M7lc1UVf-VE','http://youtu.be/M7lc1UVf-VE','https://example.com/a.mp4','https://youtube.com/watch?v=bad','/assets/a.svg','/assets/clip.mp4?x=1','\\evil\\a.mp4'])assert.equal(videoSource(url),null,url);
});
test('media insertion produces portable escaped Markdown and marks videos explicitly',()=>{
  assert.equal(attachmentMarkdown('assets/my (image).png','my [image].png','image/png'),'![my \\[image\\].png](</assets/my%20(image).png>)');
  assert.equal(attachmentMarkdown('assets/clip.mp4','clip.mp4','video/mp4'),'[clip.mp4](</assets/clip.mp4> "video")');
  assert.equal(attachmentMarkdown('assets/read.pdf','read.pdf','application/pdf'),'[read.pdf](</assets/read.pdf>)');
  assert.equal(videoMarkdown('https://youtu.be/M7lc1UVf-VE'),'[Video](<https://youtu.be/M7lc1UVf-VE> "video")');
  assert.throws(()=>videoMarkdown('https://evil.test/embed'),/YouTube/);
});
test('a video link names the outside host its player loads from, and none for notebook files',()=>{
  for(const url of ['https://www.youtube.com/watch?v=M7lc1UVf-VE','https://youtu.be/M7lc1UVf-VE','https://www.youtube-nocookie.com/embed/M7lc1UVf-VE'])assert.equal(videoHost(url),'www.youtube-nocookie.com',url);
  assert.equal(videoHost('https://vimeo.com/123456789/abcdef0123'),'player.vimeo.com');
  // This app serves notebook videos; other links make no player at all.
  for(const url of ['/assets/clip.mp4','clip.webm','https://example.com/a.mp4','https://youtube.com.evil.test/watch?v=M7lc1UVf-VE',null])assert.equal(videoHost(url),null,url);
});
