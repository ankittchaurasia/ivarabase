// Multi-file function: a relative import plus console output captured by IvaraBase.
import { greet } from "./lib/greet.ts";

export default async (req: Request) => {
  const { name = "world" } = await req.json().catch(() => ({}));
  console.log(`multi called for ${name}`);
  console.error("multi stderr line");
  return Response.json({ message: greet(name), deno: Deno.version.deno });
};
