"""{{name}} labels every item it is given with the step's label parameter.

A starting point written by agk brick init: replace the loop with what the brick does, keep its
cases under cases/ saying what it should publish, and run them with agk brick test.
agentiik_brick.py beside it is the helper, copied in whole; the brick needs nothing installed.
"""

import agentiik_brick as brick

label = brick.param("label", "")
if not label:
    # 120 is invalid input: the step will be as wrong on every attempt, so it is never retried.
    brick.refuse("no label: {{name}} writes the step's label into every item, and the step supplies it")

try:
    items = brick.items("in")
except ValueError as e:
    brick.refuse(str(e))
for item in items:
    item["data"]["label"] = label
try:
    brick.emit("out", items)
except ValueError as e:
    brick.fail(str(e))
