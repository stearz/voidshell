import datetime as dt
import unittest
from cleanup_images import deletion_plan


class RetentionTests(unittest.TestCase):
    def test_preserves_tags_recent_graphs_and_shared_children(self):
        now = dt.datetime(2026, 10, 30, 12, tzinfo=dt.timezone.utc)
        def version(name, tags=(), hours=48):
            return dict(id=name, name=name, created_at=(now-dt.timedelta(hours=hours)).isoformat(),
                        metadata={'container': {'tags': tags}})
        versions = [version('main', ['main', 'a'*40]), version('release', ['v1.0.0']),
                    version('manual', ['custom']), version('old', ['b'*40]),
                    version('new', ['c'*40], 23), version('shared'), version('platform'),
                    version('attestation'), version('recent-child'), version('orphan'),
                    version('boundary', ['d'*40], 24)]
        graph = {'main': ['shared'], 'release': ['platform', 'attestation'],
                 'platform': ['shared'], 'new': ['recent-child']}
        def manifest(name):
            return {'manifests': [{'digest': child} for child in graph.get(name, [])]}
        self.assertEqual([v['name'] for v in deletion_plan(versions, manifest, now)], ['old', 'orphan'])

    def test_registry_failure_aborts_plan(self):
        version = dict(name='release', created_at='2020-01-01T00:00:00Z',
                       metadata={'container': {'tags': ['v1']}})
        def broken(_):
            raise RuntimeError('registry unavailable')
        with self.assertRaises(RuntimeError):
            deletion_plan([version], broken, dt.datetime.now(dt.timezone.utc))


if __name__ == '__main__':
    unittest.main()
