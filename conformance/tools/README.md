# tools

Two scripts that turn probe output into the tables in the repo README. They
exist because both tables were hand-edited once, and both picked up mapping
bugs that way: `WebSearch` did not match `web_search` because the comparison
was case-sensitive, and codex's `spawn_agent` and `request_user_input` were
missing from the capability lists, so two agents were shown lacking
capabilities they have.

## capability-matrix.py

Turns `--probe tools` output into the capability matrix.

```bash
harness-test --harness all --mode headless --probe tools 2>&1 \
  | grep -o '\[tools\] [a-z]*/[a-z]* wanted "[^"]*", declared: .*' \
  | sed 's/\[tools\] \([a-z]*\)\/[a-z]*.*declared: /\1 /' > tools.txt
python3 tools/capability-matrix.py tools.txt --snapshot tests/expected/cursor-tools.json
```

`--snapshot` adds a column from a committed tool snapshot, for an agent whose
tools are not on the wire (cursor's backend chooses them). Its header carries
a `*`.

Each capability lists the tool names that provide it, most specific first, and
an agent with a shell but no dedicated tool for a file row is marked `↳shell`
rather than `—`: a bare dash meant both "has no tool for this" and "reaches it
another way", and codex read as having nothing when it does all file work
through `exec_command`.

## package-sweep.py

Reports tool-shaped strings in an agent's shipped package that no turn
declared. Run it in the container after a suite run has installed the agents:

```bash
docker run --rm --entrypoint bash \
  -v "$PWD:/work:ro" agents-installed:latest \
  -lc 'python3 /work/tools/package-sweep.py'
```

It resolves each agent's code from three shapes: an npm package under the
shared prefix, a binary the installer dropped in a bin directory, and a pip
package in site-packages. The curl and pip installers write into `$HOME`, and
the suite points `HOME` at a temp directory it deletes, so those agents have to
be installed against a fixed `HOME` before the sweep can see them — set
`SWEEP_HOME` if it is not `/home/testuser`.

The output is candidates, not measurements. It reads strings near other
strings, so parameter names and status values survive the filter, and a name
in a package may be dead code, an alias or a flagged feature. The wire is
still the only place an answer comes from.

## history-logs.sh and cmd/history

Build `harness/history/<agent>.json`, the per-version record
`harness.Compatibility` reads, from the suite's CI logs.

```bash
tools/history-logs.sh /tmp/hist        # paced; keeps KEEP=2000 API calls for others
go run ./cmd/history -index /tmp/hist/index.tsv -logs /tmp/hist/raw
```

`cmd/history` reads every result line (`✓ ✗ ○ ●`) under each run header,
names it by the runner's check ID (read from `runner/*.go`, so `acp: stop
hook fired` and `acp: stop hook not fired — …` are both `acp/hook.stop`),
and folds the runs into one outcome per check per version: the outcome at
least two thirds of that version's runs printed, else `mixed`. A skip counts
only when it says the agent cannot do the thing (`known-missing`,
`known-issue`); "the agent was never offered the tool call" and the like say
the check was not asked, and are left out.

It prints each change between two versions. `AGENT` marks a change whose two
sides ran the same suite commit, so the agent changed; `suite?` marks one
where the suite changed too, which needs checking before it goes in the
registry. GitHub keeps job logs for 90 days, so runs older than that are gone
from the API; their versions stay in the committed files.

After each nightly on main the History workflow (`.github/workflows/history.yml`)
does this for that run alone: `tools/history-logs.sh OUT RUN`, then
`cmd/history -merge`, which adds the run to the committed history. A version's
outcome for a check stays the one two thirds of its runs printed because the
runs behind each recent version are kept in `tests/history/<agent>.tally.json`;
a version not run for 14 days drops out of the tallies and keeps its recorded
outcomes. A run already counted is not counted again. The workflow pushes the
result to the branch `history/auto` (main takes pull requests only), and fails
when a change is marked `AGENT`, listing it in its summary. Merge
`history/auto` to ship the new history in a release; a version with a green
run there raises `Tested.Max` without a registry edit.
