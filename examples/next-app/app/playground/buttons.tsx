"use client";

import { useState } from "react";

export function Buttons() {
  const [result, setResult] = useState("");

  async function load(path: string) {
    const started = performance.now();
    const response = await fetch(path);
    await response.text();
    setResult(`${path}: ${response.status} in ${Math.round(performance.now() - started)} ms`);
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: ".5rem", alignItems: "flex-start" }}>
      <button
        onClick={() => {
          // Thrown from an event handler, so no error boundary sees it: the
          // browser reports it as an uncaught error.
          throw new Error("acme: the playground button threw");
        }}
      >
        Throw an error
      </button>
      <button onClick={() => void Promise.reject(new Error("acme: a promise nobody caught"))}>Reject a promise</button>
      <button onClick={() => console.error("acme: something logged with console.error", { cart: 3 })}>
        Log an error
      </button>
      <button onClick={() => load("/api/orders")}>Fetch the orders</button>
      <button onClick={() => load("/api/orders/9999")}>Fetch an order that is not there</button>
      <button onClick={() => load("/api/slow")}>Fetch something slow</button>
      <output>{result}</output>
    </div>
  );
}
