import { describe, expect, it } from "vitest";
import { greet } from "../src/index.js";

describe("greet", () => {
  it("returns a greeting for the given name", () => {
    expect(greet("Cloudbird")).toBe("Hello, Cloudbird!");
  });
});
