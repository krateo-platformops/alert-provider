"""Keep only the CustomResourceDefinitions of a rendered chart (stdin -> stdout)."""
import sys

import yaml

docs = [d for d in yaml.safe_load_all(sys.stdin) if d and d.get("kind") == "CustomResourceDefinition"]
sys.stdout.write(yaml.safe_dump_all(docs))
