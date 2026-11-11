# Collecting match data

The audit log records what the system scored, but not who was actually in
front of the camera. Without that, the similarity scores cannot be turned
into a threshold decision — you cannot tell a low score for a real person
from a low score for a stranger.

These scripts close that gap: they fire an MQTT burst, record the outcome
next to a label naming who was present, and analyse the labeled rows.

## Setup

```bash
export FACE_API_TOKEN=<the API token>
export SAMPLES_CSV=~/face-samples.csv
```

The trigger helper is a small Go command in this repository:

```bash
go build -o ~/bin/face-api-trigger ./cmd/face-api-trigger
export TRIGGER_BIN=~/bin/face-api-trigger
```

## Collecting

Walk up to the camera, then run:

```bash
scripts/collect-samples.sh Cemre        # Cemre is at the door
scripts/collect-samples.sh empty 5      # five captures of an empty scene
```

The label is what makes the data useful:

- a user's name — the person is present and enrolled
- `empty` — nobody is there (this measures the false-positive floor)

Run it several times per person: distance, angle and lighting all move the
score, and the useful threshold is the worst case, not the best one.

## Analysing

```bash
scripts/analyze-samples.py ~/face-samples.csv
```

The output has three parts:

- **per person** — for each label, the similarity range and how the system
  answered (correct name, wrong name, or missed). The minimum is the number
  that matters: it is the worst case you have to live with.
- **empty scene** — the highest score a non-enrolled face produced. This is
  the floor a threshold has to stay above.
- **threshold sweep** — for each candidate threshold, how many genuine
  samples it catches and how many empty-scene samples it wrongly accepts.
  The threshold to use is the lowest one that still catches everything while
  accepting nothing.

## Reference points

Measured with the real ONNX pipeline against a mock camera serving a real
photograph (see `internal/service/burst_e2e_test.go`):

| Situation | Similarity |
|---|---|
| Same photo (enroll and capture identical) | 0.9834 |
| Same person, different photograph | 0.5233 |
| A different person | 0.1505 |

The same person in a different photograph is the number that bounds the
threshold from above: a threshold above it would miss that person.
