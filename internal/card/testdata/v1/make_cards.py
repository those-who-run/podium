"""Write the cards v1 1.0.0 posts for a recording (paddock#66): replays it into
a store of its own as ecu's testdata/summary/make_golden.py does (no rules, v1's
RecordingClock from 2026-01-01T00:00:00Z), then runs `those-who-run post <id>
--dry-run` on each session v1 let go of (on_end), in order, with PITWALL_URL set.

    python3 make_cards.py OUT_DIR PITWALL_URL RECORDING [JOINED]

With JOINED it is the restart pair, as ecu's testdata/continue/make_golden.py's
"continue" scenario (8.7 s apart, the window 600 s).
One file per recording: OUT_DIR/NAME.txt, each card's dry-run output, then a
line `-- colour=0x... timestamp=EPOCH_SECONDS` with what the dry run doesn't
print, cards separated by a blank line.
"""

import json
import os
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

from those_who_run.callouts import CalloutQueue
from those_who_run.engine import Engine
from those_who_run.replay import read_recording
from those_who_run.results import build_card
from those_who_run.store import SessionStore

EPOCH = datetime(2026, 1, 1, tzinfo=timezone.utc).timestamp()
GAP = 8.7


class Clock:
    def __init__(self):
        self.now = EPOCH

    def __call__(self):
        return self.now


def run(states, store, clock, engine, start, release=False):
    t0 = states[0].t
    tick = start
    for state in states:
        now = start + (state.t - t0)
        if release:
            while tick + 1 <= now:
                tick += 1
                clock.now = tick
                store.release_held()
        clock.now = now
        if release:
            store.release_held()
        engine.process(state)


def main(out_dir, pitwall, rec, joined=None):
    clock = Clock()
    ended = []
    tmp = tempfile.mkdtemp()
    store = SessionStore(Path(tmp), clock=clock, on_end=lambda s: ended.append(s))
    engine = Engine([], CalloutQueue(max_age=None), store)
    run(list(read_recording(Path(rec), strict=True)), store, clock, engine, EPOCH)
    if joined:
        engine.close_session(shutdown=True)
        store.close()
        last = clock.now
        store = SessionStore(Path(tmp), clock=clock, on_end=lambda s: ended.append(s), continue_window_secs=600.0)
        engine = Engine([], CalloutQueue(max_age=None), store)
        run(list(read_recording(Path(joined), strict=True)), store, clock, engine, last + GAP, release=True)
    engine.close_session()
    store.close()
    env = dict(os.environ, GT7_DATA_DIR=tmp, PITWALL_URL=pitwall)
    parts = []
    for s in ended:
        sid = s["session_id"]
        p = subprocess.run(["those-who-run", "post", sid, "--dry-run"], env=env, capture_output=True, text=True)
        text = p.stdout
        card = build_card(s, pitwall)
        if card is not None:
            text += f"-- colour=0x{card.colour:06X} footer={card.footer} timestamp={card.timestamp.timestamp() if card.timestamp else ''}\n"
        parts.append(text)
    name = Path(rec).name.removesuffix(".jsonl.gz") if not joined else "restart-pair"
    Path(out_dir, name + ".txt").write_text("\n".join(parts), encoding="utf-8")


if __name__ == "__main__":
    main(*sys.argv[1:])
