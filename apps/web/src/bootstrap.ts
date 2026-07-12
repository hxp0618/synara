// FILE: bootstrap.ts
// Purpose: Completes synchronous renderer storage migration before any app store can hydrate.

import "./storageOriginMigration";
import { captureRemoteAccessToken } from "./remoteAccessToken";

captureRemoteAccessToken();

void import("./main");
