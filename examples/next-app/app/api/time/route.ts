// Fetched with `cache: "no-store"`: never cached.
export async function GET() {
  return Response.json({ now: new Date().toISOString() });
}
