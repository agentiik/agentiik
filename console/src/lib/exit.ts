// What an exit code means, by the band the documentation's table puts it in: the band is read off the
// code alone, since a manifest declares no exit codes.

export type Band = { name: string; tone: "succeeded" | "failed" | "waiting" | "quiet"; handling: string };

export function band(code: number): Band {
  if (code === 0) return { name: "success", tone: "succeeded", handling: "its output envelopes are published" };
  if (code >= 1 && code <= 99) return { name: "application failure", tone: "failed", handling: "not retried unless retry.on says so" };
  if (code >= 100 && code <= 119) return { name: "transient failure", tone: "waiting", handling: "retried as the step's policy says" };
  if (code === 120) return { name: "invalid input", tone: "failed", handling: "a permanent failure, never retried" };
  if (code === 121) return { name: "output contract broken", tone: "failed", handling: "set by the runner, charged to the brick, never retried" };
  if (code >= 122 && code <= 124) return { name: "reserved for the runner", tone: "failed", handling: "a brick exiting with it has failed the contract" };
  return { name: "reserved for the runtime", tone: "quiet", handling: "an infrastructure failure, charged to the runner and not to the brick" };
}
