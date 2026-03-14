# What is ZKTeco ADMS?

ADMS = Automatic Data Master Server (sometimes also referred to as Attendance Device Management System). It’s the proprietary communication protocol used by ZKTeco biometric and RFID terminals (fingerprint, face, card readers, etc.) to send real-time attendance and access logs to a central server.

The protocol is at the heart of ZKTeco’s cloud-based attendance management. Devices don’t just store logs locally — they push logs, user data, and status updates to an ADMS server over HTTP(S).

## How it Works

1. Device Initialization

Each device is configured with:

Server URL (e.g., http://server-ip/icc/ or custom endpoint)
Device serial number (SN)
Communication key (optional security)

On boot, the device registers itself with the ADMS server.

2. Communication Channel

Transport: HTTP (or HTTPS on newer firmwares).
Method: Standard HTTP POST/GET requests.
Format: Query parameters + plain-text body (not JSON/XML, mostly custom key=value pairs).

3. Request Types

Device → Server

c=registry → Device registration
c=log → Push attendance/access logs
c=options → Send device options/config
c=photo → Upload photos (if supported)

Server → Device

Returns plain text commands (OK, FAIL, USER ADD, USER DEL, etc.)
Commands are queued until device fetches them.

4. Data Format

Logs are often posted as text lines:

```
PIN=12345    DateTime=2025-09-02 14:32:11    Verified=1    Status=0
User upload/download is similar, e.g.:

PIN=12345    Name=John Doe    Privilege=0    Card=12345678
Features of the ADMS Protocol
```

Push Mechanism: Logs are sent to the server immediately (not just polled).
Two-way Sync: Server can remotely manage users, fingerprints, and settings.
Lightweight: Uses very simple HTTP/text instead of complex APIs.
Stateless: Each request is standalone, relies on SN + key for identification.
Compatible: Works across many ZKTeco device families (iClock, ZKTime, SilkBio, etc.).

## Limitations

Proprietary: Not officially published in full detail (info comes from reverse-engineering and community docs).
Security: Plain HTTP by default; encryption is weak unless forced over HTTPS.
Legacy Style: No REST/JSON standardization — integration can be messy.

ZKTeco ADMS exchange (simplified, but accurate)

1. Device Registration (first contact)

HTTP Request (from device → server)

```
POST /iclock/cdata?SN=AC123456789&table=options&c=registry HTTP/1.1
Host: yourserver.com
Content-Type: application/x-www-form-urlencoded
Content-Length: 0
```

Server Response

```
OK

```

This tells the device “you’re registered, continue sending data.”

2. Sending Attendance Logs

```
HTTP Request

POST /iclock/cdata?SN=AC123456789&table=ATTLOG&c=log HTTP/1.1
Host: yourserver.com
Content-Type: text/plain
Content-Length: 128

PIN=1001    DateTime=2025-09-02 14:32:11    Verified=1    Status=0
PIN=1002    DateTime=2025-09-02 14:35:54    Verified=1    Status=0
PIN = user ID
DateTime = timestamp of check-in/out
Verified = verification mode (1=fingerprint, 15=face, etc.)
Status = check type (0=check-in, 1=check-out, etc.)
```

Server Response

```
OK
```
3. Sending User Data

```
HTTP Request

POST /iclock/cdata?SN=AC123456789&table=USER&c=data HTTP/1.1
Host: yourserver.com
Content-Type: text/plain

PIN=1001    Name=John Doe    Privilege=0    Card=12345678
PIN=1002    Name=Alice Smith Privilege=14   Card=87654321
Privilege=0 → Normal user
Privilege=14 → Admin
```

Server Response

```
OK
```

4. Server Commands → Device

Sometimes the device polls for commands.

Device Poll

```
GET /iclock/getrequest?SN=AC123456789 HTTP/1.1
Host: yourserver.com
Server Response

USER ADD PIN=1003    Name=Bob Marley    Privilege=0    Card=99887766
USER DEL PIN=1002
```

The device will execute these and then respond "OK".

5. Photo / Face Templates (optional)

Some devices also send photos or templates with:

```
POST /iclock/cdata?SN=AC123456789&table=PHOTO&c=upload
```
(binary data in body)

As you can see, it’s just HTTP + plain text with key=value pairs, very different from REST/JSON APIs. The whole flow is lightweight and easy to parse, which is why ZKTeco can support it even on low-power terminals.

Code exemple with Python/Flask

```
from flask import Flask, request

app = Flask(__name__)

@app.route("/iclock/cdata", methods=["GET", "POST"])
def cdata():
    sn = request.args.get("SN")
    table = request.args.get("table")
    command = request.args.get("c")

    print(f"📡 Device SN={sn}, table={table}, command={command}")

    if table == "ATTLOG" and request.data:
        logs = request.data.decode("utf-8").strip().splitlines()
        for line in logs:
            print("   → Log:", line)
        return "OK"

    if table == "USER" and request.data:
        users = request.data.decode("utf-8").strip().splitlines()
        for line in users:
            print("   → User:", line)
        return "OK"

    return "OK"


@app.route("/iclock/getrequest", methods=["GET"])
def getrequest():
    sn = request.args.get("SN")
    print(f"📡 Device {sn} is polling for commands")

    # Example: tell device to add a user
    return "USER ADD PIN=2001\tName=Test User\tPrivilege=0\tCard=12345678"


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8000, debug=True)
```

What this server does

Accepts logs (ATTLOG)
Accepts user sync (USER)
Returns OK to acknowledge
Responds to getrequest with commands (like adding/removing users)

This is enough to get a ZKTeco device talking to your server. In production you’d typically:

Save logs/users in a database
Implement authentication (commKey)
Use HTTPS
Add an admin dashboard


Original 

[Link-1][https://www.linkedin.com/pulse/zkteco-adms-protocol-link-your-zk-device-server-herbin-tsobeng-qg0ze/]
[Link-2][https://stackoverflow.com/questions/65844119/zkteco-push-sdk/72994156]
