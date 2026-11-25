import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from canary import Client, check_case


class CanaryTests(unittest.TestCase):
    def setUp(self):
        self.calls = []
        calls = self.calls

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                calls.append((self.path, body, self.headers["Authorization"]))
                result = {"data": {"rows": [[7]]}, "receipt": {"complete": True}}
                if self.path.endswith("list_data_operations"):
                    result = [{"name": "query", "readOnly": True, "schemaHash": "current-contract"}]
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps(result).encode())

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever)
        self.thread.start()
        self.client = Client(f"http://127.0.0.1:{self.server.server_port}", "synthetic-token")

    def tearDown(self):
        self.server.shutdown()
        self.thread.join()
        self.server.server_close()

    def test_live_read_binds_schema_and_checks_evidence(self):
        check_case(self.client, {"action": "call_data_operation", "arguments": {
            "connection": "warehouse", "operation": "query", "arguments": {"sql": "SELECT 7"}},
            "assertions": [{"pointer": "/data/rows/0/0", "equals": 7}, {"pointer": "/receipt/complete", "equals": True}]})
        self.assertEqual(len(self.calls), 2)
        self.assertEqual(self.calls[-1][1]["schemaHash"], "current-contract")
        self.assertEqual(self.calls[-1][2], "Bearer synthetic-token")

    def test_canary_cannot_write_or_trigger_recipe_maintenance(self):
        for case in [{"action": "save_note"}, {"action": "call_data_operation", "arguments": {
            "connection": "warehouse", "operation": "query", "profilePath": "recipe.md", "profileRevision": "old"}}]:
            with self.assertRaises(ValueError):
                check_case(self.client, case)
        self.assertEqual(self.calls, [])

    def test_wrong_evidence_fails(self):
        with self.assertRaises(ValueError):
            check_case(self.client, {"action": "read_document", "assertions": [{"pointer": "/data/rows/0/0", "equals": 8}]})


if __name__ == "__main__":
    unittest.main()
