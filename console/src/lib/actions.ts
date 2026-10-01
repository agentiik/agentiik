import { refusal, type API } from "../api/client";

// What a principal holding workflow:run on a run's workflow may do to it from the inspector: ask for
// it to be cancelled while it goes, and replay it once it is over, from a step or from its start.

// cancelRun asks the controller to end the run. The API answers that it was asked, the same whether
// the run is going or has ended, so how it stands is read again after.
export async function cancelRun(api: API, run: string): Promise<void> {
  const { response, error } = await api.POST("/api/v1/runs/{id}/cancel", { params: { path: { id: run } } });
  if (response.status !== 202) {
    throw refusal(response, error);
  }
}

// replayRun starts a new run of the commit the run pinned, from step or, where none is named, from
// the start, and answers the new run.
export async function replayRun(api: API, run: string, step?: string): Promise<string> {
  const { data, response, error } = await api.POST("/api/v1/runs/{id}/replay", {
    params: { path: { id: run } },
    body: step === undefined ? {} : { step },
  });
  if (!data) {
    throw refusal(response, error);
  }
  return data.run;
}
