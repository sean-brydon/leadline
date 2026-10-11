// A made-up exchange rate. The products page fetches it with
// `next: { revalidate: 60 }`, so most renders get it from Next's data cache.
export async function GET() {
  return Response.json({ eur: 0.92 });
}
