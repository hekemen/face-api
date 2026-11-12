# Match quality analysis — 2026-11-12

Measured against the live system on the Pi (Helm rev 31, image
`sha256:538787862…`) after the 5 MP burst pipeline went live, plus the
enrolled data in the database.

All numbers come from the real ONNX pipeline (SCRFD + ArcFace) run locally
against the camera's actual frames and the actual stored pictures.

## 1. The pipeline works

| Check | Result |
|---|---|
| Burst sample resolution | **2560×1920** (logged per sample) |
| CPU per trigger | ~1.64 core-seconds (~21% of one core) |
| Peak instantaneous CPU | ~200% of one core (2 samples overlap) |
| Idle CPU | 0% (no background stream) |
| Memory | 232 MiB idle → 320 MiB peak → back to 232 MiB, no leak |
| RTP packet loss | zero |
| Pod restarts over 10 h | 0 |

## 2. Overnight results (33 bursts, 2026-11-11 23:53 → 2026-11-12 10:21)

- 31 `no_face`, 2 `not_matched`, 0 `matched`.
- **All 31 `no_face` verdicts were correct.** The detector was re-run over
  every stored frame: best face score 0.09–0.30, none reached the 0.5
  detection threshold. Nobody was at the camera.
- The 2 `not_matched` verdicts reproduce exactly when recomputed locally
  (audit 0.2262 → local 0.2273; audit 0.2822 → local 0.2835).

## 3. The threshold is above genuine matches for these users

Similarity measurements over the **enrolled users' own pictures** (each
user's stored crops against each other, and against a known stranger):

| Comparison | Range |
|---|---|
| Same person, two photos | **0.068 – 0.375** |
| Known stranger vs. their enrollments | **−0.060 – 0.131** |
| Two captured faces (different people) | 0.055 |

Control: a known-good pair of the same person (Anthony Hopkins photos 1
and 2) scores **0.5318** — so the measurement method is sound, and the real
users' same-person scores are genuinely low.

The two faces detected overnight scored **0.2262** and **0.2822** — inside
the same-person band and well above the stranger band. They were most
likely enrolled users, not strangers, and the threshold rejected them.

**Consequence: the current 0.30 threshold produces false negatives for
these users, and the historical 0.30–0.43 "matches" were a mixture of
genuine hits and wrong ones.**

Re-running the detector over the 86 historical `matched` crops for the real
users: only 20–48% of the pairs of crops that the system matched to a given
user are actually the same face (a genuine pair scores ≥0.25). The rest are
different people being assigned that name.

## 4. Why the same-person scores are low

The captured face size is not the limiter:

| Face size in a 2560×1920 frame | Similarity to a clean enrollment |
|---|---|
| 100 px | 0.6558 |
| 200 px | 0.8146 |
| 400 px | 0.8549 |

And the enrollment's source resolution barely matters either (enrolling from
a 112 px crop vs. a 50 px crop: 0.8882 vs 0.8726 for the same query).

The crops are sharp (mean gradient 8–25, matching the known-good control's
13–17). The stored crops are 112×112 and byte-identical in one case
(`serap` pic3 and pic4 are the same file, so one of the five "pictures" for
`serap` is a duplicate).

**The crop alignment is the limiter.** Comparing the two alignment modes on
the same photos:

| Mode | Same person | Different person | Separation |
|---|---|---|---|
| `bbox` (current default) | 0.5244 | 0.1587 | 0.3657 |
| **`arcface5`** | **0.6252** | **0.0685** | **0.5567** |

`arcface5` warps the five detected landmarks onto the ArcFace template, so
the crop is aligned regardless of the head's pose in the frame. `bbox` crops
the raw detection box, so the face lands in the 112×112 crop at a different
scale and offset every time — which is exactly what produces a low
same-person score across photos.

`arcface5` is already implemented and reachable via `FACE_ALIGNMENT`, but
it changes the embedding space, so **switching it requires re-enrolling every
user**.

## 5. What the numbers say about the threshold

With the current (bbox) enrollments, a threshold must sit:

- **below ~0.07** to catch every genuine match, or
- **above ~0.13** to reject every stranger.

Those do not overlap, so no single threshold separates them with the current
enrollments. That is the underlying problem, and it is an enrollment-quality
problem, not a threshold-tuning problem.

Switching to `arcface5` moves the same-person band up (0.52 → 0.63 against
the known-good control) and the stranger band down (0.16 → 0.07), which
widens the gap a threshold can live in.

## 6. Recommended next steps

1. **Switch `FACE_ALIGNMENT=arcface5` and re-enroll everyone** from good
   5 MP stills (face filling the frame, well lit, front-on). This is the
   change that actually fixes the separation.
2. **Re-enroll with several photos per person** (the current set includes a
   duplicate for `serap`, and a single bad photo dominates a user's score).
3. Then re-measure with `scripts/collect-samples.sh` and pick the threshold
   from the sweep in `scripts/analyze-samples.py`.
4. Only then raise the threshold above 0.30.

Re-enrolling is needed regardless: the burst pipeline now captures at 5 MP,
and the existing enrollments were made from lower-resolution or
poorly-aligned sources, so they are not comparable to new captures.
