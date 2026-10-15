import importlib.util
from pathlib import Path
import tempfile
import unittest
spec = importlib.util.spec_from_file_location("maintenance", Path(__file__).parents[1] / "maintenance_transaction.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

class MaintenanceFailureTests(unittest.TestCase):
    def fixture(self, root, stuck=False, ambiguous=False):
        state = {"fleets": {"web": {"name": "web", "instances": 2, "minimum_available": 1},
                            "db": {"name": "db", "instances": 1, "minimum_available": 0}},
                 "assignments": {n+str(i): {"fleet": n, "state": "ACTIVE"}
                                 for n,f in {"web":{"instances":2},"db":{"instances":1}}.items()
                                 for i in range(f["instances"])}}
        stops = []
        def scale(name, count):
            state["fleets"][name]["instances"] = count
            state["fleets"][name]["minimum_available"] = min(count, state["fleets"][name]["minimum_available"])
            state["assignments"] = {key:a for key,a in state["assignments"].items() if a["fleet"] != name}
            if ambiguous:
                raise TimeoutError("response lost after commit")
            if stuck:
                state["assignments"]["db-1"] = {"fleet": "db", "state": "STOPPED"}
        def put(f):
            state["fleets"][f["name"]] = f
            state["assignments"] = {key:a for key,a in state["assignments"].items() if a["fleet"] != f["name"]}
            for i in range(f["instances"]):
                state["assignments"][f["name"]+str(i)] = {"fleet": f["name"], "state": "ACTIVE"}
        m = module.Maintenance(Path(root)/"journal.json", lambda: state, scale, put,
                               stop_services=lambda: stops.append(True), timeout=0, poll=0)
        return m, state, stops

    def test_stopped_writer_timeout_restores_production_and_policy(self):
        with tempfile.TemporaryDirectory() as root:
            m,s,stops = self.fixture(root, stuck=True)
            with self.assertRaisesRegex(RuntimeError, "did not quiesce"):
                m.quiesce(["web","db"])
            self.assertEqual((s["fleets"]["web"]["instances"],s["fleets"]["web"]["minimum_available"]),(2,1))
            self.assertEqual(s["fleets"]["db"]["instances"],1)
            self.assertFalse(stops)

    def test_ambiguous_scale_commit_restored_without_post_replay(self):
        with tempfile.TemporaryDirectory() as root:
            m,s,_ = self.fixture(root, ambiguous=True)
            with self.assertRaises(TimeoutError):
                m.quiesce(["web","db"])
            self.assertEqual(s["fleets"]["web"]["instances"],2)
            self.assertEqual(s["fleets"]["db"]["instances"],1)

    def test_offline_action_failure_and_interruption_restore(self):
        for failure in (RuntimeError("backup failed"), KeyboardInterrupt()):
            with tempfile.TemporaryDirectory() as root:
                m,s,_ = self.fixture(root)
                def action():
                    raise failure
                with self.assertRaises(type(failure)):
                    m.run(["web","db"], action)
                self.assertEqual(s["fleets"]["web"]["minimum_available"],1)
                self.assertEqual(s["fleets"]["db"]["instances"],1)

    def test_unfinished_journal_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as root:
            m,s,_ = self.fixture(root)
            m.quiesce(["web","db"])
            with self.assertRaisesRegex(RuntimeError,"unfinished"):
                m.quiesce(["web","db"])
            m.resume()
            self.assertEqual(s["fleets"]["web"]["instances"],2)
            self.assertEqual(m.path.stat().st_mode & 0o777,0o600)

    def test_watchdog_recovers_orphaned_journal_and_ignores_live_owner(self):
        with tempfile.TemporaryDirectory() as root:
            m,s,_ = self.fixture(root)
            m.quiesce(["web","db"])
            self.assertFalse(m.recover_if_orphaned())
            import json
            j = json.loads(m.path.read_text())
            j["owner_pid"] = 2147483647
            m.save(j)
            self.assertTrue(m.recover_if_orphaned())
            self.assertEqual(s["fleets"]["web"]["instances"],2)

    def test_resume_retry_does_not_start_another_rollout(self):
        with tempfile.TemporaryDirectory() as root:
            m,s,_ = self.fixture(root)
            calls = []
            put = m.put_fleet
            def counted(f):
                calls.append(f["name"])
                put(f)
            m.put_fleet = counted
            m.quiesce(["web","db"])
            def unavailable():
                raise RuntimeError("not ready yet")
            m.verify = unavailable
            with self.assertRaisesRegex(RuntimeError, "not ready"):
                m.resume()
            self.assertEqual(calls, ["web","db"])
            m.verify = lambda: None
            m.resume()
            self.assertEqual(calls, ["web","db"])
