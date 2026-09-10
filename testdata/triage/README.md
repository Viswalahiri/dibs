# Golden fixtures

Each file here is one issue dibs has already scored, with a band a human
assigned by hand. `make eval` re-scores all of them against a live model and
asserts they land in the band they were given.

Bands, never exact scores. The numbers move whenever the weights are retuned,
and a test that pinned them would need rewriting every time calibration
improved. What has to hold is that an issue worth claiming stays worth
claiming.

## Where a fixture comes from

The set is a byproduct of the calibration week, not something to invent. Run
dibs, let it score real issues, then pick the ones whose verdict you have an
opinion about. `dibs status --today` lists what is available and
`dibs status --missed` lists what the floor killed, which is where the
interesting disagreements are.

The `input` field is that issue's `triage_input` column copied verbatim, so the
model sees exactly the bytes it saw in production:

```sql
SELECT triage_input FROM issues WHERE id = ?;
```

## Format

```json
{
  "name": "author says a patch is coming",
  "band": "veto",
  "veto": "self_fixing",
  "note": "third comment is the maintainer saying PR incoming",
  "repo": { "slug": "acme/widget", "receptivity": "normal", "stacks": ["go"] },
  "labels": ["bug"],
  "input": "REPOSITORY: acme/widget\n..."
}
```

`band` is one of `veto`, `low`, `mid`, `high`. `veto` names which of the three
should fire and is required for a vetoed fixture. `note` is why you gave it
that band, and it is printed when the run disagrees with you.

## What the run needs

At least twelve fixtures, and one covering each of the three vetoes. Below
that, the run fails rather than passing quietly: an eval that agrees with you
because there is nothing to disagree with is worse than no eval at all.
