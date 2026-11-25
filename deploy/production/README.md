# Production release

These tools build, verify and publish a committed image: the release gate
([releasing images](releasing.md)), `release_image.py` with its verification plan
and receipts, the `fast` pull-request check and the canary. They deploy nothing.
A single host or Compose stack deploys an image as [deploying ForgetMeNote](../../docs/deployment.md)
describes. A hosted instance keeps its deployment tools, settings, records and
pipeline in its own deployment repository, outside this one; those tools run
from a checkout of this repository and use this release gate.
