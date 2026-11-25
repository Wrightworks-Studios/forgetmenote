---
title: Live preview stress note
tags: [editor, markdown, stress]
kind: test-note
---

# Live preview stress note

A synthetic note that uses every Markdown construct the editor has to show. Nothing in it is personal. Edit anything: the text you see **is** the document, and the status bar names every change.

## Inline formatting

Plain text with *emphasis*, _underscore emphasis_, **strong**, __underscore strong__, ***both at once***, ~~struck through~~, and `inline code`. Code with a backtick inside: `` a`b ``. Nested: **strong with *emphasis* inside** and *emphasis with `code` inside*.

Escapes keep markers literal: \*not emphasis\*, \_not either\_, \# not a heading, \[not a link\], a backslash \\ and a pipe \|. Entities: &amp; &copy; &#8212; &lt;tag&gt;.

A hard break with two trailing spaces  
and one with a backslash\
then the paragraph goes on. Unicode: naïve café, 日本語, emoji 🌿, and right-to-left עברית.

## Headings

### Third level

#### Fourth level with a closing sequence ####

##### Fifth level

###### Sixth level

Setext heading, level one
=========================

Setext heading, level two
-------------------------

## Links

An [inline link](https://example.com/path?q=1 "With a title"), a [reference link][ref], a [collapsed reference][], an autolink <https://example.org>, a bare URL https://example.net/page and an email <someone@example.com>.

Page links: [[Getting started]], [[Project plan|the plan]] and [[Notes/Sub page]]. Square brackets that are no link: [just text] and [x].

[ref]: https://example.com/reference "Reference title"
[collapsed reference]: https://example.com/collapsed

## Lists

- First bullet
- Second bullet with **strong** text
  - Nested bullet
    - Third level bullet
      - Fourth level
  - Back to the second level
- Third bullet that is long enough to wrap onto a second line when the editor is narrow, so wrapping inside list items can be checked.

* Star bullets
+ Plus bullets

1. First
2. Second
   1. Nested ordered
   2. Another
      - Mixed bullet inside ordered
3. Third

7. A list that starts at seven
8. And continues

## Tasks

- [ ] An open task
- [x] A done task
- [X] Done with a capital X
- [ ] A task with **formatting**, `code` and a [link](https://example.com)
  - [ ] A nested open task
  - [x] A nested done task
1. [ ] An ordered task
2. [x] Another ordered task

## Quotes

> A block quote with *emphasis*.
> It goes on for a second line.
>
> > A nested quote
> > with two lines.
>
> - A list in a quote
> - [ ] A task in a quote

> A quote whose second line is lazy
continuing without a marker.

## Code

```js
// A fenced block with a language.
function greet(name) {
  return `Hello, ${name}!`; // **not bold**, [not a link](x)
}
```

```
A fence without a language.
    Indentation is kept.
```

~~~python
def tilde_fence():
    return "~~~ inside is fine"
~~~

````markdown
```
A fence inside a longer fence.
```
````

    An indented code block,
    four spaces deep.

## Tables

| Left | Centre | Right | Formatting |
|:-----|:------:|------:|------------|
| a | b | c | **strong** and *em* |
| 1 | 2 | 3 | `code with \| pipe` |
| long cell text that keeps going | | 42 | [link](https://example.com) |
| missing | cells |

Without outer pipes:

Name | Value
--- | ---
alpha | 1
beta | 2

## Rules

Above a rule.

---

***

___

Below the rules.

## Images

![A small SVG drawing](data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIyNDAiIGhlaWdodD0iOTYiIHZpZXdCb3g9IjAgMCAyNDAgOTYiPjxyZWN0IHdpZHRoPSIyNDAiIGhlaWdodD0iOTYiIHJ4PSIxMCIgZmlsbD0iI2UwZWRmMyIvPjxjaXJjbGUgY3g9IjQ4IiBjeT0iNDgiIHI9IjI2IiBmaWxsPSIjMjQ2YWEyIi8+PHJlY3QgeD0iOTIiIHk9IjMwIiB3aWR0aD0iMTIwIiBoZWlnaHQ9IjEyIiByeD0iNiIgZmlsbD0iIzVhNjU1ZiIvPjxyZWN0IHg9IjkyIiB5PSI1NCIgd2lkdGg9Ijg0IiBoZWlnaHQ9IjEyIiByeD0iNiIgZmlsbD0iIzg1OGY4NiIvPjwvc3ZnPg==)

An inline PNG gradient ![gradient](data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAADAAAAAQCAIAAADfzGvmAAAFbklEQVR42g3OocqyPQCA4f9wvmAwGAwGg8FgMBgMBoPBIMOwIA+GBRmGBXkwLMgwLMiNYUGGYUGGYcFgMBgMBg/kf68juP77J2gImoKWoC3oCLqCnqAvGAiGgpFgLJgIpoKZYC5YCJYCKVgJ1gIl2Ai2AiPYCfYCKzgIjgIvOAnOgiC4CK6CJLgJ7oIieAiegpfgLfgIvoKf4L9/koakKWlJ2pKOpCvpSfqSgWQoGUnGkolkKplJ5pKFZCmRkpVkLVGSjWQrMZKdZC+xkoPkKPGSk+QsCZKL5CpJkpvkLimSh+QpeUneko/kK/nJv1BFo6JZ0apoV3QquhW9in7FoGJYMaoYV0wqphWzinnFomJZIStWFesKVbGp2FaYil3FvsJWHCqOFb7iVHGuCBWXimtFqrhV3CtKxaPiWfGqeFd8Kr4Vv+ovpGgomoqWoq3oKLqKnqKvGCiGipFirJgopoqZYq5YKJYKqVgp1gql2Ci2CqPYKfYKqzgojgqvOCnOiqC4KK6KpLgp7oqieCieipfirfgovoqf+gtpGpqmpqVpazqarqan6WsGmqFmpBlrJpqpZqaZaxaapUZqVpq1Rmk2mq3GaHaavcZqDpqjxmtOmrMmaC6aqyZpbpq7pmgemqfmpXlrPpqv5qf/QoaGoWloGdqGjqFr6Bn6hoFhaBgZxoaJYWqYGeaGhWFpkIaVYW1Qho1hazCGnWFvsIaD4WjwhpPhbAiGi+FqSIab4W4ohofhaXgZ3oaP4Wv4mb9QTaOmWdOqadd0aro1vZp+zaBmWDOqGddMaqY1s5p5zaJmWSNrVjXrGlWzqdnWmJpdzb7G1hxqjjW+5lRzrgk1l5prTaq51dxrSs2j5lnzqnnXfGq+Nb/6L2RpWJqWlqVt6Vi6lp6lbxlYhpaRZWyZWKaWmWVuWViWFmlZWdYWZdlYthZj2Vn2Fms5WI4WbzlZzpZguViulmS5We6WYnlYnpaX5W35WL6Wn/0LORqOpqPlaDs6jq6j5+g7Bo6hY+QYOyaOqWPmmDsWjqVDOlaOtUM5No6twzh2jr3DOg6Oo8M7To6zIzgujqsjOW6Ou6M4Ho6n4+V4Oz6Or+Pn/kKehqfpaXnano6n6+l5+p6BZ+gZecaeiWfqmXnmnoVn6ZGelWftUZ6NZ+sxnp1n77Geg+fo8Z6T5+wJnovn6kmem+fuKZ6H5+l5ed6ej+fr+fm/EDSgCS1oQwe60IM+DGAIIxjDBKYwgzksYAkSVrAGBRvYgoEd7MHCAY7g4QRnCHCBKyS4wR0KPOAJL3jDB77w4y8UaASagVagHegEuoFeoB8YBIaBUWAcmASmgVlgHlgElgEZWAXWARXYBLYBE9gF9gEbOASOAR84Bc6BELgEroEUuAXugRJ4BJ6BV+Ad+AS+gV/4C0UakWakFWlHOpFupBfpRwaRYWQUGUcmkWlkFplHFpFlREZWkXVERTaRbcREdpF9xEYOkWPER06RcyRELpFrJEVukXukRB6RZ+QVeUc+kW/kF/9CiUaimWgl2olOopvoJfqJQWKYGCXGiUlimpgl5olFYpmQiVVinVCJTWKbMIldYp+wiUPimPCJU+KcCIlL4ppIiVviniiJR+KZeCXeiU/im/ilv1CmkWlmWpl2ppPpZnqZfmaQGWZGmXFmkplmZpl5ZpFZZmRmlVlnVGaT2WZMZpfZZ2zmkDlmfOaUOWdC5pK5ZlLmlrlnSuaReWZemXfmk/lmfvkvVGgUmoVWoV3oFLqFXqFfGBSGhVFhXJgUpoVZYV5YFJYFWVgV1gVV2BS2BVPYFfYFWzgUjgVfOBXOhVC4FK6FVLgV7oVSeBSehVfhXfgUvoVf4X/g5GFMOYGFiwAAAABJRU5ErkJggg==) in the middle of a sentence.

![An image that does not exist](missing/picture.png)

[![Linked image](data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAADAAAAAQCAIAAADfzGvmAAAFbklEQVR42g3OocqyPQCA4f9wvmAwGAwGg8FgMBgMBoPBIMOwIA+GBRmGBXkwLMgwLMiNYUGGYUGGYcFgMBgMBg/kf68juP77J2gImoKWoC3oCLqCnqAvGAiGgpFgLJgIpoKZYC5YCJYCKVgJ1gIl2Ai2AiPYCfYCKzgIjgIvOAnOgiC4CK6CJLgJ7oIieAiegpfgLfgIvoKf4L9/koakKWlJ2pKOpCvpSfqSgWQoGUnGkolkKplJ5pKFZCmRkpVkLVGSjWQrMZKdZC+xkoPkKPGSk+QsCZKL5CpJkpvkLimSh+QpeUneko/kK/nJv1BFo6JZ0apoV3QquhW9in7FoGJYMaoYV0wqphWzinnFomJZIStWFesKVbGp2FaYil3FvsJWHCqOFb7iVHGuCBWXimtFqrhV3CtKxaPiWfGqeFd8Kr4Vv+ovpGgomoqWoq3oKLqKnqKvGCiGipFirJgopoqZYq5YKJYKqVgp1gql2Ci2CqPYKfYKqzgojgqvOCnOiqC4KK6KpLgp7oqieCieipfirfgovoqf+gtpGpqmpqVpazqarqan6WsGmqFmpBlrJpqpZqaZaxaapUZqVpq1Rmk2mq3GaHaavcZqDpqjxmtOmrMmaC6aqyZpbpq7pmgemqfmpXlrPpqv5qf/QoaGoWloGdqGjqFr6Bn6hoFhaBgZxoaJYWqYGeaGhWFpkIaVYW1Qho1hazCGnWFvsIaD4WjwhpPhbAiGi+FqSIab4W4ohofhaXgZ3oaP4Wv4mb9QTaOmWdOqadd0aro1vZp+zaBmWDOqGddMaqY1s5p5zaJmWSNrVjXrGlWzqdnWmJpdzb7G1hxqjjW+5lRzrgk1l5prTaq51dxrSs2j5lnzqnnXfGq+Nb/6L2RpWJqWlqVt6Vi6lp6lbxlYhpaRZWyZWKaWmWVuWViWFmlZWdYWZdlYthZj2Vn2Fms5WI4WbzlZzpZguViulmS5We6WYnlYnpaX5W35WL6Wn/0LORqOpqPlaDs6jq6j5+g7Bo6hY+QYOyaOqWPmmDsWjqVDOlaOtUM5No6twzh2jr3DOg6Oo8M7To6zIzgujqsjOW6Ou6M4Ho6n4+V4Oz6Or+Pn/kKehqfpaXnano6n6+l5+p6BZ+gZecaeiWfqmXnmnoVn6ZGelWftUZ6NZ+sxnp1n77Geg+fo8Z6T5+wJnovn6kmem+fuKZ6H5+l5ed6ej+fr+fm/EDSgCS1oQwe60IM+DGAIIxjDBKYwgzksYAkSVrAGBRvYgoEd7MHCAY7g4QRnCHCBKyS4wR0KPOAJL3jDB77w4y8UaASagVagHegEuoFeoB8YBIaBUWAcmASmgVlgHlgElgEZWAXWARXYBLYBE9gF9gEbOASOAR84Bc6BELgEroEUuAXugRJ4BJ6BV+Ad+AS+gV/4C0UakWakFWlHOpFupBfpRwaRYWQUGUcmkWlkFplHFpFlREZWkXVERTaRbcREdpF9xEYOkWPER06RcyRELpFrJEVukXukRB6RZ+QVeUc+kW/kF/9CiUaimWgl2olOopvoJfqJQWKYGCXGiUlimpgl5olFYpmQiVVinVCJTWKbMIldYp+wiUPimPCJU+KcCIlL4ppIiVviniiJR+KZeCXeiU/im/ilv1CmkWlmWpl2ppPpZnqZfmaQGWZGmXFmkplmZpl5ZpFZZmRmlVlnVGaT2WZMZpfZZ2zmkDlmfOaUOWdC5pK5ZlLmlrlnSuaReWZemXfmk/lmfvkvVGgUmoVWoV3oFLqFXqFfGBSGhVFhXJgUpoVZYV5YFJYFWVgV1gVV2BS2BVPYFfYFWzgUjgVfOBXOhVC4FK6FVLgV7oVSeBSehVfhXfgUvoVf4X/g5GFMOYGFiwAAAABJRU5ErkJggg==)](https://example.com)

## Footnotes

A sentence with a footnote[^1] and another[^long].

[^1]: The first footnote.
[^long]: A longer footnote with *formatting*
    and a second, indented line.

## HTML

<details>
<summary>An HTML block</summary>

Markdown inside an HTML block.

</details>

<div class="note">
A raw <strong>div</strong> block.
</div>

Inline HTML: <kbd>Ctrl</kbd> + <kbd>S</kbd>, H<sub>2</sub>O and x<sup>2</sup>.

<!-- An HTML comment that should stay out of the way. -->

## Edge cases

**Unclosed strong and *unclosed emphasis

A line ending with two asterisks**

Trailing backslash at the very end of a paragraph \

Tabs	inside	text and a line with only spaces below.
   
The end.
