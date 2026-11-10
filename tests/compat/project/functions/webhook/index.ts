export default async (req: Request) => {
  const body = await req.text();
  if (body.includes("boom")) throw new Error("kaboom");
  return Response.json({ ok: true, received: body.length });
};
