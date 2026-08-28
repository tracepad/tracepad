// A pure SPA (spec 006 Decision 1): the Go binary serves static files and the
// browser does all the routing. Nothing is prerendered, because every page is
// a view of data only the running server has.
export const ssr = false;
export const prerender = false;
