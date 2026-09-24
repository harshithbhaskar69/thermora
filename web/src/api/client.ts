import createClient from "openapi-fetch";
import type { paths, components } from "./schema.gen";

export const api = createClient<paths>({ credentials: "same-origin" });
export type Building = components["schemas"]["Building"];
export type Device = components["schemas"]["Device"];
export type Session = components["schemas"]["Session"];
export type Sample = components["schemas"]["Sample"];
export type Command = components["schemas"]["Command"];

export class APIError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message);
  }
}

export function unwrap<T>(result: {
  data?: T;
  error?: unknown;
  response: Response;
}): T {
  if (!result.response.ok) {
    const error = result.error as { detail?: string } | undefined;
    throw new APIError(
      error?.detail ?? "The service is unavailable. Try again shortly.",
      result.response.status,
    );
  }
  return result.data as T;
}

export function newRequest() {
  return { "Idempotency-Key": crypto.randomUUID() };
}
