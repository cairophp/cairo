<?php
/**
 * Cairo\Net: TCP, UDP and Unix sockets and DNS lookups through Go.
 */

declare(strict_types=1);

namespace Cairo\Net {

    use Cairo\Internal\Go;

    /**
     * Connect to $address over Go's net package. $network: tcp, tcp4, tcp6,
     * unix, udp, udp4, udp6, or tls (TCP + TLS client). In a request
     * handler or async() task only the fiber waits.
     *
     * Options: timeout (seconds for the connect, default 30), and for tls
     * the same "ssl" options as PHP stream contexts: verify_peer,
     * verify_peer_name, allow_self_signed, peer_name, cafile, capath,
     * local_cert, local_pk, passphrase, SNI_server_name.
     *
     * @param array<string, mixed> $options
     */
    function dial(string $network, string $address, array $options = []): Conn
    {
        $tls = $options;
        unset($tls['timeout']);
        $r = Net::call('net.dial', [
            'network' => $network,
            'address' => $address,
            'timeout' => (float) ($options['timeout'] ?? 30),
            'tls' => $network === 'tls' ? $tls + ['verify_peer' => true, 'verify_peer_name' => true] : null,
        ]);
        return new Conn((int) $r['id'], (string) $r['local'], (string) $r['remote']);
    }

    /**
     * Listen for connections. $network: tcp, tcp4, tcp6, unix, or tls (pass
     * cert, and key unless the cert file holds it). Port 0 picks a free one.
     *
     * @param array{cert?: string, key?: string} $options
     */
    function listen(string $network, string $address, array $options = []): Listener
    {
        $r = Net::call('net.listen', [
            'network' => $network,
            'address' => $address,
            'cert' => (string) ($options['cert'] ?? ''),
            'key' => (string) ($options['key'] ?? ''),
        ]);
        return new Listener((int) $r['id'], (string) $r['address']);
    }

    /** A datagram socket: udp, udp4, udp6 or unixgram. */
    function listenPacket(string $network, string $address): PacketConn
    {
        $r = Net::call('net.listenPacket', ['network' => $network, 'address' => $address]);
        return new PacketConn((int) $r['id'], (string) $r['address']);
    }

    /** @internal */
    final class Net
    {
        /** @param array<string, mixed> $payload */
        public static function call(string $op, array $payload): array
        {
            $r = Go::call($op, $payload);
            if (!is_array($r) || !($r['ok'] ?? false)) {
                $kind = (string) ($r['kind'] ?? 'io');
                $message = (string) ($r['error'] ?? 'network error');
                throw $kind === 'timeout' ? new TimeoutException($message, $kind) : new NetException($message, $kind);
            }
            return $r;
        }

        /** Closing never needs to wait, so it does not suspend (it may run in a destructor). */
        public static function close(int $id, string $mode = ''): void
        {
            \cairo_go_call('net.close', ['id' => $id, 'mode' => $mode]);
        }
    }

    /**
     * A stream connection. Reads and writes wait for at most the timeout set
     * with setTimeout() (none by default), and for the request's deadline.
     */
    final class Conn
    {
        private bool $closed = false;
        private float $timeout = 0;

        /** @internal */
        public function __construct(
            private readonly int $id,
            public readonly string $localAddress,
            public readonly string $remoteAddress,
        ) {
        }

        public function __destruct()
        {
            $this->close();
        }

        /** Timeout in seconds for each read and write; null or 0 for none. */
        public function setTimeout(?float $seconds): static
        {
            $this->timeout = (float) $seconds;
            return $this;
        }

        /** Up to $max bytes, as soon as any arrive; '' at end of stream. */
        public function read(int $max = 65536): string
        {
            return $this->readMode('some', $max)[0];
        }

        /** Exactly $length bytes; throws NetException (kind "eof") if the peer closes first. */
        public function readExactly(int $length): string
        {
            [$data, $eof] = $this->readMode('exact', $length);
            if ($eof) {
                throw new NetException(sprintf('connection closed after %d of %d bytes', strlen($data), $length), 'eof');
            }
            return $data;
        }

        /**
         * Up to and including $eol (at most $max bytes); the rest of the
         * stream if it ends first, or null at end of stream.
         */
        public function readLine(string $eol = "\n", int $max = 65536): ?string
        {
            [$data, $eof] = $this->readMode('line', $max, $eol);
            return $eof && $data === '' ? null : $data;
        }

        /** Writes all of $data; returns its length. */
        public function write(string $data): int
        {
            $r = Net::call('net.write', ['id' => $this->id, 'data' => base64_encode($data), 'timeout' => $this->timeout]);
            return (int) $r['written'];
        }

        /** Shut down the writing side: the peer reads end of stream. */
        public function closeWrite(): void
        {
            Net::call('net.close', ['id' => $this->id, 'mode' => 'write']);
        }

        public function close(): void
        {
            if (!$this->closed) {
                $this->closed = true;
                Net::close($this->id);
            }
        }

        /** @return array{0: string, 1: bool} data and whether the stream ended */
        private function readMode(string $mode, int $max, string $eol = "\n"): array
        {
            $r = Net::call('net.read', ['id' => $this->id, 'mode' => $mode, 'max' => $max, 'eol' => $eol, 'timeout' => $this->timeout]);
            return [base64_decode((string) $r['data']), (bool) ($r['eof'] ?? false)];
        }
    }

    final class Listener
    {
        private bool $closed = false;

        /** @internal */
        public function __construct(private readonly int $id, public readonly string $address)
        {
        }

        public function __destruct()
        {
            $this->close();
        }

        /** The next connection; waits at most $timeout seconds (null: no limit). */
        public function accept(?float $timeout = null): Conn
        {
            $r = Net::call('net.accept', ['id' => $this->id, 'timeout' => (float) $timeout]);
            return new Conn((int) $r['id'], (string) $r['local'], (string) $r['remote']);
        }

        public function close(): void
        {
            if (!$this->closed) {
                $this->closed = true;
                Net::close($this->id);
            }
        }
    }

    final class PacketConn
    {
        private bool $closed = false;

        /** @internal */
        public function __construct(private readonly int $id, public readonly string $address)
        {
        }

        public function __destruct()
        {
            $this->close();
        }

        /** @return array{0: string, 1: string} the datagram and the sender's address */
        public function readFrom(int $max = 65536, ?float $timeout = null): array
        {
            $r = Net::call('net.readFrom', ['id' => $this->id, 'max' => $max, 'timeout' => (float) $timeout]);
            return [base64_decode((string) $r['data']), (string) $r['from']];
        }

        public function writeTo(string $data, string $address, ?float $timeout = null): int
        {
            $r = Net::call('net.writeTo', ['id' => $this->id, 'data' => base64_encode($data), 'address' => $address, 'timeout' => (float) $timeout]);
            return (int) $r['written'];
        }

        public function close(): void
        {
            if (!$this->closed) {
                $this->closed = true;
                Net::close($this->id);
            }
        }
    }

    /**
     * DNS through Go's resolver. Pass a server ("1.1.1.1:53") to ask it
     * instead of the system's resolver. Lookups of names that do not exist
     * throw NetException with kind "not_found".
     */
    final class Resolver
    {
        public function __construct(private readonly ?string $server = null, private readonly float $timeout = 10)
        {
        }

        /** @return list<string> IPv4 and IPv6 addresses */
        public function lookupHost(string $host): array
        {
            return $this->lookup('host', $host);
        }

        /** @return list<string> */
        public function lookupIPv4(string $host): array
        {
            return $this->lookup('ip4', $host);
        }

        /** @return list<string> */
        public function lookupIPv6(string $host): array
        {
            return $this->lookup('ip6', $host);
        }

        /** @return list<string> names for an address */
        public function lookupAddr(string $ip): array
        {
            return $this->lookup('addr', $ip);
        }

        public function lookupCNAME(string $host): string
        {
            return $this->lookup('cname', $host);
        }

        /** @return list<string> */
        public function lookupTXT(string $name): array
        {
            return $this->lookup('txt', $name);
        }

        /** @return list<array{host: string, pref: int}> by preference */
        public function lookupMX(string $name): array
        {
            return $this->lookup('mx', $name);
        }

        /** @return list<string> */
        public function lookupNS(string $name): array
        {
            return $this->lookup('ns', $name);
        }

        /**
         * _$service._$proto.$name; with '' for both, $name is looked up as is.
         *
         * @return list<array{target: string, port: int, priority: int, weight: int}>
         */
        public function lookupSRV(string $service, string $proto, string $name): array
        {
            return $this->lookup('srv', $name, ['service' => $service, 'proto' => $proto])['records'];
        }

        /** @param array<string, string> $extra */
        private function lookup(string $kind, string $name, array $extra = []): mixed
        {
            return Net::call('net.lookup', ['kind' => $kind, 'name' => $name, 'server' => (string) $this->server, 'timeout' => $this->timeout] + $extra)['result'];
        }
    }

    class NetException extends \RuntimeException
    {
        /** $kind: dial, tls, listen, timeout, closed, eof, io, not_found or lookup */
        public function __construct(string $message, public readonly string $kind = 'io')
        {
            parent::__construct($message);
        }
    }

    final class TimeoutException extends NetException
    {
    }
}
