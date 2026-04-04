export default {
  async fetch(request, env) {
    const cors = {
      "Access-Control-Allow-Origin": "*",
      "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
      "Content-Type": "application/json",
    };
    if (request.method === "OPTIONS") return new Response(null, { headers: cors });

    const url = new URL(request.url);

    if (url.pathname === "/increment" && request.method === "POST") {
      const val = parseInt(await env.COUNTER.get("configs") || "0") + 1;
      await env.COUNTER.put("configs", val.toString());
      return new Response(JSON.stringify({ count: val }), { headers: cors });
    }

    if (url.pathname === "/count" && request.method === "GET") {
      const val = parseInt(await env.COUNTER.get("configs") || "0");
      return new Response(JSON.stringify({ count: val }), { headers: cors });
    }

    return new Response(JSON.stringify({ error: "not found" }), { status: 404, headers: cors });
  },
};
