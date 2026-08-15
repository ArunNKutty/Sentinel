/** @type {import('next').NextConfig} */
const nextConfig = {
  // The capture routes stream pcap for as long as the user watches, so they
  // must never be statically analysed or cached.
  experimental: {
    proxyTimeout: 0,
  },
};

export default nextConfig;
