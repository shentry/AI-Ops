"""Safety checks for the isolated load runner; no Docker/SSH/network required."""
import copy
import unittest

from run import safety


class SafetyTest(unittest.TestCase):
    def setUp(self):
        self.row = {
            "available_memory_bytes": 2 * 1024**3,
            "disk_free_bytes": 4 * 1024**3,
            "sub2api_status": 200,
            "sub2api_health_ms": 2,
            "queue": {"pending": 0, "oldest_pending_s": 0, "failed": 0},
            "mysql": {"Threads_connected": 5},
            "containers": {"mysql": {
                "memory_bytes": 500 * 1024**2,
                "memory_working_set_bytes": 400 * 1024**2,
                "memory_limit": 768 * 1024**2,
                "memory_events": {"oom_kill": 0},
            }},
            "host_cpu_ticks": [100, 0, 10, 200, 0, 0, 0, 0],
        }

    def test_healthy_baseline(self):
        self.assertEqual(safety(self.row, None), "")

    def test_reclaimable_file_cache_does_not_trip_memory_guard(self):
        # Measured at the interrupted AWS stage: ~731 MiB total, ~289 MiB inactive file.
        memory = self.row["containers"]["mysql"]
        memory["memory_bytes"] = 766078976
        memory["memory_working_set_bytes"] = 766078976 - 302694400
        self.assertEqual(safety(self.row, None), "")

    def test_full_working_set_and_oom_stop_load(self):
        for key, value in (("memory_working_set_bytes", 768 * 1024**2), ("oom_kill", 1)):
            with self.subTest(key=key):
                row = copy.deepcopy(self.row)
                memory = row["containers"]["mysql"]
                if key == "oom_kill":
                    memory["memory_events"][key] = value
                else:
                    memory[key] = value
                    memory["memory_bytes"] = value
                self.assertTrue(safety(row, None))

    def test_host_and_protected_service_limits(self):
        for key, value in (("available_memory_bytes", 100), ("disk_free_bytes", 100),
                           ("sub2api_status", 503), ("sub2api_health_ms", 1001)):
            with self.subTest(key=key):
                row = copy.deepcopy(self.row)
                row[key] = value
                self.assertTrue(safety(row, None))

    def test_queue_and_connection_limits(self):
        for group, key, value in (("queue", "pending", 5001), ("queue", "oldest_pending_s", 61),
                                  ("queue", "failed", 1), ("mysql", "Threads_connected", 81)):
            with self.subTest(key=key):
                row = copy.deepcopy(self.row)
                row[group][key] = value
                self.assertTrue(safety(row, None))

    def test_sustained_host_cpu_limit(self):
        previous = copy.deepcopy(self.row)
        previous["host_busy_pct"] = 95
        self.row["host_cpu_ticks"][0] += 100
        self.row["host_cpu_ticks"][3] += 1
        self.assertTrue(safety(self.row, previous))


if __name__ == "__main__":
    unittest.main()
