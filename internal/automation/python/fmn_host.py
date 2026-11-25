"""Version 1 host client. No retries, network transport, credentials or providers."""
import json
import sys

_OUT, _IN = sys.stdout, sys.stdin

class HostError(RuntimeError):
    pass

class Host:
    def call(self, step, capability, **arguments):
        body = {'step': step, 'operation': capability, 'arguments': arguments}
        _OUT.write(json.dumps(body, allow_nan=False) + '\n')
        _OUT.flush()
        line = _IN.readline()
        if not line:
            raise HostError('ForgetMeNote closed the host connection')
        reply = json.loads(line)
        if 'error' in reply:
            raise HostError(reply['error'])
        return reply['result']
