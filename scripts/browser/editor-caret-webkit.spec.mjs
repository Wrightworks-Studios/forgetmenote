// The note editor's caret and selection can be seen, and the caret blinks,
// in WebKit, Safari's engine, which draws the browser's own caret its own
// way (see editor-caret.mjs for the cases). WebKit launches without the
// configuration's Chromium-only options.
import {test} from '@playwright/test';
import {caretCases} from './editor-caret.mjs';

test.use({browserName: 'webkit', launchOptions: {}});
caretCases('WebKit');
