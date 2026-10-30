"""Remove old dev manifests, preserving all retained manifest dependencies."""
import datetime as dt
import json
import os
import re
import subprocess


def command(*args):
    return subprocess.check_output(args, text=True)


def api(path, *args):
    return json.loads(command('gh', 'api', path, *args))


def deletion_plan(versions, manifest, now):
    cutoff = now - dt.timedelta(hours=24)
    protected = set()

    def preserve(digest):
        if digest in protected:
            return
        protected.add(digest)
        for child in manifest(digest).get('manifests', []):
            preserve(child['digest'])

    for version in versions:
        tags = version['metadata']['container']['tags']
        created = dt.datetime.fromisoformat(version['created_at'].replace('Z', '+00:00'))
        # Only full commit-SHA tags and orphaned untagged manifests are disposable.
        # Preserve main, latest, releases, and any unfamiliar/manual tags.
        if created >= cutoff or any(not re.fullmatch(r'[0-9a-f]{40}', tag) for tag in tags):
            preserve(version['name'])

    return [v for v in versions if v['name'] not in protected]


def main():
    owner, repository = os.environ['GITHUB_REPOSITORY'].split('/')
    scope = 'orgs' if api(f'users/{owner}')['type'] == 'Organization' else 'users'
    now = dt.datetime.now(dt.timezone.utc)
    plans = []
    for package in (repository.lower(), repository.lower() + '-workspace'):
        endpoint = f'{scope}/{owner}/packages/container/{package}/versions'
        pages = api(endpoint + '?per_page=100', '--paginate', '--slurp')
        versions = [v for page in pages for v in page]
        image = f'ghcr.io/{owner.lower()}/{package}'

        def manifest(digest):
            return json.loads(command('docker', 'buildx', 'imagetools', 'inspect',
                                      '--raw', f'{image}@{digest}'))

        # Resolve every protected graph before deleting anything. API or registry
        # errors abort the run; never interpret a failed read as an empty graph.
        plans.append((endpoint, deletion_plan(versions, manifest, now)))

    for endpoint, versions in plans:
        for version in versions:
            print(f"Delete {endpoint}/{version['id']} ({version['name']})", flush=True)
            command('gh', 'api', '--method', 'DELETE', f"{endpoint}/{version['id']}")


if __name__ == '__main__':
    main()
