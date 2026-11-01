// {{name}} labels every item it is given with the step's label parameter.
//
// A starting point written by agk brick init: replace the loop with what the brick does, keep its
// cases under cases/ saying what it should publish, and run them with agk brick test. brick.ts
// beside it is the helper, copied in whole; Node runs both as they are, with no build and nothing
// installed.

import * as brick from "./brick.ts";

const label = brick.param<string>("label");
if (!label) {
  // 120 is invalid input: the step will be as wrong on every attempt, so it is never retried.
  brick.refuse("no label: {{name}} writes the step's label into every item, and the step supplies it");
}

let items: brick.Item[] = [];
try {
  items = brick.items("in");
} catch (err) {
  brick.refuse((err as Error).message);
}
for (const item of items) item.data.label = label;
try {
  brick.emit("out", items);
} catch (err) {
  brick.fail((err as Error).message);
}
