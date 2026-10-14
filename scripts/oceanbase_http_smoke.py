#!/usr/bin/env python3
"""Exercise authenticated Wiki HTTP APIs on the isolated local PoC.

Creates synthetic fixtures, leaves the resulting KB for review, and writes
only redacted evidence to build. Credentials/session are private Git-ignored
files. Does not call a language model or claim automatic ingest coverage.
"""
import json
import os
from pathlib import Path
import secrets
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
BASE = 'http://127.0.0.1:18080/api/v1'

def private(path, text):
    with open(path, 'w') as out:
        os.chmod(path, 0o600)
        out.write(text)

login_path = ROOT / '.env.oceanbase-poc-login'
if not login_path.exists():
    private(login_path, 'POC_EMAIL=ob-poc@example.invalid\nPOC_USERNAME=OceanBase PoC\nPOC_PASSWORD=' + secrets.token_urlsafe(24) + '\n')
login = dict(line.split('=', 1) for line in login_path.read_text().splitlines() if '=' in line)
token = ''
checks = []
def request(method, path, body=None, expected=(200,)):
    data = None if body is None else json.dumps(body, ensure_ascii=False).encode()
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=120) as res:
            raw = res.read()
            status, result = res.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as error:
        status, result = error.code, json.load(error)
    if status not in expected:
        # Auth bodies can contain credentials: expose only structured errors.
        raise RuntimeError(f'{method} {path}: {status}, error={result.get("error", result.get("message", "unexpected status"))}')
    checks.append({'method': method, 'path': path, 'status': status})
    print(method, path, status, flush=True)
    return result, status

payload = {'email': login['POC_EMAIL'], 'password': login['POC_PASSWORD']}
session, status = request('POST', '/auth/login', payload, (200, 401))
if status == 401:
    request('POST', '/auth/register', dict(payload, username=login.get('POC_USERNAME', 'OceanBase PoC')), (200, 201))
    session, _ = request('POST', '/auth/login', payload)
token = session['token']
private(ROOT / '.oceanbase-poc-session.json', json.dumps(session))
kb, _ = request('POST', '/knowledge-bases', {'name': 'OceanBase HTTP 验收 ' + uuid.uuid4().hex[:6], 'type': 'wiki', 'indexing_strategy': {'vector_enabled': False, 'keyword_enabled': False, 'wiki_enabled': True, 'graph_enabled': False}, 'wiki_config': {}} , (200, 201))
kb = kb.get('data', kb)
kbid = kb['id']
path = '/knowledgebase/' + kbid + '/wiki'
folder, _ = request('POST', path + '/folders', {'name': '数据库'}, (201,))
page, _ = request('POST', path + '/pages', {'slug': 'entity/oceanbase-poc', 'title': 'OceanBase PoC', 'content': '# OceanBase\n\n中文正文与版本验证。', 'summary': '数据库存储验证', 'page_type': 'entity', 'status': 'published', 'aliases': ['OB'], 'source_refs': [], 'folder_id': folder['id']}, (201,))
request('GET', path + '/pages/' + page['slug'])
updated, _ = request('PUT', path + '/pages/' + page['slug'], {'content': '# OceanBase\n\n第二版正文。', 'version': page['version']})
request('PUT', path + '/pages/' + page['slug'], {'content': '过期更新', 'version': page['version']}, (409,))
revisions, _ = request('GET', path + '/revisions/' + page['slug'])
assert revisions['total'] >= 1
request('GET', path + '/pages?query=OCEANBASE')
request('GET', path + '/folders')
for endpoint in ['/index', '/graph', '/stats', '/search?q=OceanBase', '/lint']:
    request('GET', path + endpoint)
# Preserve the primary page; delete/recreate a second fixture for inspection.
request('POST', path + '/pages', {'slug': 'concept/recreate', 'title': '重建验证', 'content': 'first', 'page_type': 'concept'}, (201,))
request('DELETE', path + '/pages/concept/recreate', expected=(200, 204))
request('POST', path + '/pages', {'slug': 'concept/recreate', 'title': '重建验证', 'content': 'second', 'page_type': 'concept'}, (201,))
(ROOT / 'build/oceanbase-http-evidence.json').write_text(json.dumps({'knowledge_base_id': kbid, 'checks': checks, 'automatic_ingest_tested': False}, ensure_ascii=False, indent=2) + '\n')
print('Wiki HTTP smoke passed. Review KB:', kbid)
