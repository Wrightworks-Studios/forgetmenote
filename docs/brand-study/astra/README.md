# ForgetMeNote flower study

Four native SVG concepts, each with a transparent `0 0 64 64` viewBox and the same blue (`#7298ee`) and gold (`#edc16c`). These are review assets; the production app and Fraunces wordmark are unchanged.

| File | Direction | Judgment |
| --- | --- | --- |
| [01-corolla.svg](01-corolla.svg) | Full, gently flattened petals with open rounded clefts and a plain gold center. | The closest continuation of the original. More legible as five petals, but less distinctive than Field. |
| [02-field.svg](02-field.svg) | Five individually drawn petals, unequal shoulders, and a scalloped golden throat around a blue aperture. | **Recommended.** The asymmetry gives the mark character while the flower remains immediately readable. |
| [03-open.svg](03-open.svg) | Separate, broad petals taper toward a larger gold center. | The clearest separation and strongest center. Airy and tidy, although the repeated petals feel more formal. |
| [04-pressed.svg](04-pressed.svg) | Flatter crowns and rounded corners, with a slight overall rotation. | A more graphic, printed character. The furthest from a natural flower. |

I would carry Field forward and refine the throat against the exact header and favicon backgrounds. Its blue aperture is distinct at 32 px and becomes a hint at 16 px; the larger flower silhouette still holds. Open is the alternative if simple separation and a stronger gold center matter more than botanical character.

Rendered and visually inspected at 16, 24, 32, and 192 px. All four SVGs parse as XML and contain only native vector shapes. No production app changes or automated application tests were needed for this concept study.
