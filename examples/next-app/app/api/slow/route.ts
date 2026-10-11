// Takes a second and a half, to show up as a slow fetch in the browser.
export async function GET() {
  await new Promise((resolve) => setTimeout(resolve, 1500));
  return Response.json({ slow: true });
}
