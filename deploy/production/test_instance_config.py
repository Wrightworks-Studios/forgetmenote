"""Keep the public operator examples aligned with actual startup controls."""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]


class InstanceConfigurationContracts(unittest.TestCase):
    def test_compose_and_example_expose_supported_integration_controls(self):
        source = (ROOT / "internal/instanceconfig/config.go").read_text()
        block = source.split("var LegacyEnvironment =", 1)[1].split("\n}", 1)[0]
        controls = set(re.findall(r'"([A-Z_]+)"', block)) | {"INSTANCE_CONFIG_FILE"}
        example = (ROOT / ".env.example").read_text()
        compose = (ROOT / "compose.yaml").read_text()
        example_keys = set(re.findall(r"^([A-Z_]+)=", example, re.MULTILINE))
        compose_keys = set(re.findall(r"^      ([A-Z_]+):", compose, re.MULTILINE))
        self.assertFalse(controls - example_keys, sorted(controls - example_keys))
        self.assertFalse(controls - compose_keys, sorted(controls - compose_keys))
        self.assertNotIn("MCP_ACCESS_CONFIG_FILE", example_keys)

    def test_compose_preserves_the_installed_docker_python_default(self):
        docker = (ROOT / "Dockerfile").read_text()
        default = re.search(r"AUTOMATION_PYTHON=([^\s]+)", docker).group(1)
        config = (ROOT / "internal/instanceconfig/config.go").read_text()
        canonical = re.search(r'const DockerPython = "([^"]+)"', config).group(1)
        self.assertEqual(canonical, default)
        self.assertIn("AUTOMATION_PYTHON: ${AUTOMATION_PYTHON:-" + default + "}",
                      (ROOT / "compose.yaml").read_text())


if __name__ == "__main__":
    unittest.main()
