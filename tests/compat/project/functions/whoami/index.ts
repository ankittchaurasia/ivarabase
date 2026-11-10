import { createClient } from "npm:@supabase/supabase-js@2.46.1";

Deno.serve(async (req) => {
  const supabase = createClient(Deno.env.get("SUPABASE_URL")!, Deno.env.get("SUPABASE_ANON_KEY")!, {
    global: { headers: { Authorization: req.headers.get("Authorization")! } },
  });
  const { data: { user } } = await supabase.auth.getUser();
  const { data: todos } = await supabase.from("todos").select("title");
  return Response.json({ email: user?.email ?? null, todos, greeting: Deno.env.get("GREETING") });
});
