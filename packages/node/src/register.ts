// The side-effect entry: `node --import @leadline/node/register app.js`, or
// `import "@leadline/node/register"` as the app's first line. Configured by
// environment variables alone; for options, call register() from "@leadline/node".
import { register } from "./index.js";

register();
