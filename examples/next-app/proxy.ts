// Next 16's proxy (the file that used to be middleware.ts). It only stamps a
// header, to show up in Leadline as its own step of the request.
import { NextResponse, type NextRequest } from "next/server";

export function proxy(_request: NextRequest) {
  const response = NextResponse.next();
  response.headers.set("x-acme-shop", "1");
  return response;
}

export const config = { matcher: ["/((?!_next/|favicon.ico).*)"] };
