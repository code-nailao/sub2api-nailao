#!/usr/bin/env python3
"""Require the latest native push CI for the exact repository/commit, not PR CI."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from sources_baseline import load_identity


def select_run(runs, repository, sha):
    valid = [run for run in runs if run['event'] == 'push' and run['head_sha'] == sha
             and (run.get('head_repository') or {}).get('full_name') == repository]
    if not valid:
        return None
    return max(valid, key=lambda run: (int(run['id']), int(run['run_attempt'])))


def evaluate(run):
    if run is None or run['status'] != 'completed':
        return 'wait'
    if run['conclusion'] != 'success':
        raise ValueError('latest push CI failed: run='+str(run['id'])+' conclusion='+str(run['conclusion']))
    return 'pass'


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--repository', default=load_identity()['repository'])
    p.add_argument('--sha', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--timeout', type=int, default=1800)
    a = p.parse_args()
    assert re.fullmatch('[a-f0-9]{40}', a.sha)
    output = Path(a.output)
    assert not output.exists(), 'preserve earlier CI receipt'
    start = time.monotonic()
    while time.monotonic()-start < a.timeout:
        raw = subprocess.check_output(['gh', 'api', '--method', 'GET',
              'repos/'+a.repository+'/actions/workflows/backend-ci.yml/runs',
              '-f', 'head_sha='+a.sha, '-f', 'event=push', '-f', 'per_page=100'])
        run = select_run(json.loads(raw)['workflow_runs'], a.repository, a.sha)
        if evaluate(run) == 'pass':
            # Bind the current attempt, not a stale successful attempt.
            detail = json.loads(subprocess.check_output(['gh','api','repos/'+a.repository+'/actions/runs/'+str(run['id'])]))
            if detail['run_attempt'] != run['run_attempt'] or evaluate(detail) != 'pass':
                continue
            assert select_run([detail], a.repository, a.sha) == detail
            receipt = {key:detail[key] for key in ('id','run_attempt','head_sha','event','status','conclusion','html_url','updated_at')}
            receipt['repository'] = a.repository
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_text(json.dumps(receipt, indent=2)+'\n')
            print('CI_GATE_PASS run='+str(detail['id'])+' attempt='+str(detail['run_attempt'])+' source='+a.sha)
            return
        time.sleep(30)
    raise TimeoutError('latest push CI is not complete; no publication')


if __name__ == '__main__':
    main()
