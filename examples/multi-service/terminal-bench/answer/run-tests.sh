#!/bin/sh
if python -c 'import urllib.request; assert urllib.request.urlopen("http://api:8080", timeout=3).read() == b"42"'; then
  result=PASSED
else
  result=FAILED
fi
echo '================ short test summary info ================'
echo "$result tests/test_api.py::test_answer"
