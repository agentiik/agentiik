// The types of tests/serve.js, for the screen tests written in TypeScript.

export type Scenario = Record<string, { status: number; body?: unknown; text?: string }>;

export function consolePolicy(): string;

export function scenarioNamed(name: string): Scenario;

export function serve(options: { scenario: Scenario; port?: number; prefix?: string }): Promise<{
  url: string;
  state: { scenario: Scenario; asked: string[] };
  close: () => Promise<void>;
}>;
