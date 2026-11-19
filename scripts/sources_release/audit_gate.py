#!/usr/bin/env python3
"""Distinguish a completed vulnerability audit from a registry/network error."""
import argparse
import json
from pathlib import Path
import subprocess


def validate_report(value):
    if not isinstance(value,dict) or 'error' in value or not isinstance(value.get('advisories'),dict):
        raise ValueError('package audit did not return a complete advisory report')
    vulnerabilities=(value.get('metadata') or {}).get('vulnerabilities')
    if not isinstance(vulnerabilities,dict) or not all(k in vulnerabilities and isinstance(vulnerabilities[k],int) for k in ('low','moderate','high','critical')):
        raise ValueError('package audit lacks vulnerability summary')


def main():
    p=argparse.ArgumentParser();p.add_argument('--output',required=True);a=p.parse_args()
    result=subprocess.run(['corepack','pnpm@9.15.9','--dir','frontend','audit','--prod','--audit-level=high','--json'],capture_output=True)
    path=Path(a.output);path.write_bytes(result.stdout)
    if result.returncode not in (0,1):raise RuntimeError('audit command failed: '+str(result.returncode))
    validate_report(json.loads(result.stdout))
    print('AUDIT_REPORT_COMPLETE')

if __name__=='__main__':main()
