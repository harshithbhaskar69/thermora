import { describe, expect, it } from "vitest";
import { APIError, unwrap } from "./client";

describe("API boundary", () => {
  it("preserves actionable authorization errors", () => {
    expect(() =>
      unwrap({
        error: { detail: "The resource is unavailable." },
        response: new Response(null, { status: 404 }),
      }),
    ).toThrow(APIError);
    expect(() =>
      unwrap({ response: new Response(null, { status: 503 }) }),
    ).toThrow("The service is unavailable.");
  });
  it("returns a typed success without inventing state", () => {
    expect(
      unwrap({
        data: { stage: "accepted" },
        response: new Response(null, { status: 202 }),
      }),
    ).toEqual({ stage: "accepted" });
  });
});
